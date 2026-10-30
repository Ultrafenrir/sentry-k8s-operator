/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package telegramdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

func TestWorkerCompletesSuccessfulDelivery(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claim: testRecord()}
	routes := &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "-1001", TopicIdentity: "2"}}
	sender := &fakeSender{}
	worker := testWorker(queue, routes, sender)

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed || queue.completed != 1 || queue.failed != 0 || sender.calls != 1 || sender.target != routes.target || sender.text != queue.claim.Text {
		t.Fatalf("processed=%t err=%v queue=%+v sender=%+v", processed, err, queue, sender)
	}
}

func TestWorkerRedeliversAfterSuccessfulSendAndFailedSettlement(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	queue := &delivery.Queue{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "delivery",
		MaxOutstanding: 10, MaxAttempts: 3, DedupeRetention: time.Hour, DLQRetention: 24 * time.Hour,
		Now: func() time.Time { return now },
	}
	message := delivery.Message{
		DedupeKey: strings.Repeat("b", 64), RouteKey: strings.Repeat("c", 64), Text: "summary",
	}
	if _, created, err := queue.Enqueue(context.Background(), message); err != nil || !created {
		t.Fatalf("enqueue = (created=%t, error=%v)", created, err)
	}
	settlementFailure := errors.New("injected Kubernetes settlement failure")
	store := &failFirstCompleteQueue{Queue: queue, err: settlementFailure}
	sender := &fakeSender{}
	observer := &recordingObserver{}
	worker := &Worker{
		Queue: store, Routes: &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "-1001"}},
		Sender: sender, Owner: strings.Repeat("a", 64), Lease: time.Minute,
		RetryBase: time.Second, RetryMax: time.Minute, Observer: observer,
		Jitter: func(value time.Duration) time.Duration { return value },
		Audit:  &recordingAudit{},
	}

	processed, err := worker.ProcessOne(context.Background())
	if !processed || !errors.Is(err, settlementFailure) || sender.calls != 1 {
		t.Fatalf("first processing = (processed=%t, error=%v, sends=%d)", processed, err, sender.calls)
	}
	now = now.Add(time.Minute)
	processed, err = worker.ProcessOne(context.Background())
	if !processed || err != nil || sender.calls != 2 {
		t.Fatalf("recovered processing = (processed=%t, error=%v, sends=%d)", processed, err, sender.calls)
	}
	statistics, err := queue.Statistics(context.Background())
	if err != nil || statistics.ByState[delivery.StateSent] != 1 {
		t.Fatalf("final queue statistics = (%+v, %v)", statistics, err)
	}
	if len(observer.results) != 2 || observer.results[0].Outcome != OutcomeError || observer.results[1].Outcome != OutcomeSent {
		t.Fatalf("observed results = %+v", observer.results)
	}
}

func TestAuditFailureBeforeSendBlocksTelegramAndSettlement(t *testing.T) {
	queue := &fakeQueue{claim: testRecord()}
	sender := &fakeSender{}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "1"}}, sender)
	worker.Audit = &recordingAudit{failAt: 1}

	processed, err := worker.ProcessOne(context.Background())
	if !processed || !errors.Is(err, ErrAuditUnavailable) || sender.calls != 0 || queue.completed != 0 || queue.failed != 0 {
		t.Fatalf("audit outage did not fail closed: processed=%t err=%v sender=%+v queue=%+v", processed, err, sender, queue)
	}
}

func TestAuditFailureAfterTelegramSuccessLeavesClaimForAtLeastOnceRecovery(t *testing.T) {
	queue := &fakeQueue{claim: testRecord()}
	sender := &fakeSender{}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "1"}}, sender)
	worker.Audit = &recordingAudit{failAt: 2}

	processed, err := worker.ProcessOne(context.Background())
	if !processed || !errors.Is(err, ErrAuditUnavailable) || sender.calls != 1 || queue.completed != 0 || queue.failed != 0 {
		t.Fatalf("post-send audit outage settled an uncertain delivery: processed=%t err=%v sender=%+v queue=%+v", processed, err, sender, queue)
	}
}

func TestDeliveryAuditDoesNotExposeProtectedMessageOrRoute(t *testing.T) {
	queue := &fakeQueue{claim: testRecord()}
	auditSink := &recordingAudit{}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "-1001", TopicIdentity: "2"}}, &fakeSender{})
	worker.Audit = auditSink

	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("delivery failed: processed=%t err=%v", processed, err)
	}
	if len(auditSink.events) != 2 || auditSink.events[0].Reason != audit.ReasonDeliveryClaimed || auditSink.events[1].Reason != audit.ReasonDeliveryAccepted {
		t.Fatalf("unexpected audit events: %#v", auditSink.events)
	}
	payload, err := json.Marshal(auditSink.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{queue.claim.DedupeKey, queue.claim.RouteKey, queue.claim.Text, "-1001", "\"2\""} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("delivery audit leaked protected value %q: %s", forbidden, payload)
		}
	}
}

func TestWorkerObserverUsesOnlyFixedOutcomeAndFailureEnums(t *testing.T) {
	t.Parallel()
	observer := &recordingObserver{}
	queue := &fakeQueue{claim: testRecord()}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "1"}}, &fakeSender{
		err: &SendError{Failure: delivery.FailureRateLimited, RetryAfter: time.Second},
	})
	worker.Observer = observer
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("processed = %t, error = %v", processed, err)
	}
	if len(observer.results) != 1 || observer.results[0].Outcome != OutcomeRetry || observer.results[0].Failure != delivery.FailureRateLimited || observer.results[0].Duration < 0 {
		t.Fatalf("results = %+v", observer.results)
	}
}

func TestWorkerReturnsIdleWithoutClaim(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claimErr: delivery.ErrNoClaim}
	worker := testWorker(queue, &fakeRoutes{}, &fakeSender{})
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || processed || queue.completed != 0 || queue.failed != 0 {
		t.Fatalf("processed=%t err=%v queue=%+v", processed, err, queue)
	}
}

func TestWorkerDeadLettersRevokedRoute(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claim: testRecord()}
	routes := &fakeRoutes{err: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "route")}
	worker := testWorker(queue, routes, &fakeSender{})
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed || queue.failed != 1 || queue.failure != delivery.FailureInvalidRoute || !queue.permanent {
		t.Fatalf("processed=%t err=%v queue=%+v", processed, err, queue)
	}
}

func TestWorkerHonorsTelegramRetryAfter(t *testing.T) {
	t.Parallel()
	queue := &fakeQueue{claim: testRecord()}
	sender := &fakeSender{err: &SendError{Failure: delivery.FailureRateLimited, RetryAfter: 45 * time.Second}}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "1"}}, sender)
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed || queue.failure != delivery.FailureRateLimited || queue.permanent || queue.retryAfter != 45*time.Second {
		t.Fatalf("processed=%t err=%v queue=%+v", processed, err, queue)
	}
}

func TestWorkerUsesBoundedBackoffWithoutLeakingUnknownError(t *testing.T) {
	t.Parallel()
	record := testRecord()
	record.Attempts = 20
	queue := &fakeQueue{claim: record}
	sender := &fakeSender{err: errors.New("secret upstream response")}
	worker := testWorker(queue, &fakeRoutes{target: binding.TelegramTarget{ChatIdentity: "1"}}, sender)
	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed || queue.failure != delivery.FailureTransport || queue.permanent || queue.retryAfter != time.Minute {
		t.Fatalf("processed=%t err=%v queue=%+v", processed, err, queue)
	}
}

func TestSecureJitterIsBounded(t *testing.T) {
	t.Parallel()
	base := time.Minute
	for range 100 {
		value := secureJitter(base)
		if value < 54*time.Second || value > 66*time.Second {
			t.Fatalf("jitter %s outside ±10%%", value)
		}
	}
}

func testWorker(queue *fakeQueue, routes *fakeRoutes, sender *fakeSender) *Worker {
	return &Worker{
		Queue: queue, Routes: routes, Sender: sender, Owner: strings.Repeat("a", 64),
		Lease: time.Minute, RetryBase: time.Second, RetryMax: time.Minute,
		Jitter: func(value time.Duration) time.Duration { return value },
		Audit:  &recordingAudit{},
	}
}

func testRecord() delivery.Record {
	return delivery.Record{
		DedupeKey: strings.Repeat("b", 64), RouteKey: strings.Repeat("c", 64), Text: "summary",
		State: delivery.StateInFlight, ResourceVersion: "2", Revision: 2,
	}
}

type fakeQueue struct {
	claim      delivery.Record
	claimErr   error
	completed  int
	failed     int
	failure    delivery.Failure
	retryAfter time.Duration
	permanent  bool
	settleErr  error
}

type failFirstCompleteQueue struct {
	*delivery.Queue
	err error
}

func (q *failFirstCompleteQueue) Complete(ctx context.Context, record delivery.Record) (delivery.Record, error) {
	if q.err != nil {
		err := q.err
		q.err = nil
		return delivery.Record{}, err
	}
	return q.Queue.Complete(ctx, record)
}

func (q *fakeQueue) Claim(context.Context, string, time.Duration) (delivery.Record, error) {
	return q.claim, q.claimErr
}

func (q *fakeQueue) Complete(context.Context, delivery.Record) (delivery.Record, error) {
	q.completed++
	return delivery.Record{}, q.settleErr
}

func (q *fakeQueue) Fail(_ context.Context, _ delivery.Record, failure delivery.Failure, retryAfter time.Duration, permanent bool) (delivery.Record, error) {
	q.failed++
	q.failure = failure
	q.retryAfter = retryAfter
	q.permanent = permanent
	return delivery.Record{}, q.settleErr
}

type fakeRoutes struct {
	target binding.TelegramTarget
	err    error
}

func (r *fakeRoutes) GetTelegramRoute(context.Context, string) (binding.TelegramTarget, error) {
	return r.target, r.err
}

type fakeSender struct {
	calls  int
	target binding.TelegramTarget
	text   string
	err    error
}

type recordingObserver struct {
	results []Result
}

type recordingAudit struct {
	events []audit.Event
	failAt int
}

func (a *recordingAudit) Record(_ context.Context, event audit.Event) error {
	if a.failAt > 0 && len(a.events)+1 == a.failAt {
		return errors.New("audit sink unavailable")
	}
	if err := audit.Validate(event); err != nil {
		return err
	}
	a.events = append(a.events, event)
	return nil
}

func (o *recordingObserver) Observe(result Result) {
	o.results = append(o.results, result)
}

func (s *fakeSender) Send(_ context.Context, target binding.TelegramTarget, text string) error {
	s.calls++
	s.target = target
	s.text = text
	return s.err
}
