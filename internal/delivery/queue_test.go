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

package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
)

func TestEnqueueIsIdempotentAndDetectsConflict(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 10)
	message := testMessage("a", "b", "safe summary")

	first, created, err := queue.Enqueue(context.Background(), message)
	if err != nil || !created || first.State != StatePending {
		t.Fatalf("first enqueue = (%+v, %t, %v)", first, created, err)
	}
	second, created, err := queue.Enqueue(context.Background(), message)
	if err != nil || created || second.ResourceVersion != first.ResourceVersion {
		t.Fatalf("idempotent enqueue = (%+v, %t, %v)", second, created, err)
	}
	changed := message
	changed.Text = "different summary"
	if _, _, err := queue.Enqueue(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed enqueue error = %v, want ErrConflict", err)
	}
	if strings.Contains(recordName(message.DedupeKey), message.Text) || strings.Contains(recordName(message.DedupeKey), message.RouteKey) {
		t.Fatal("object name contains protected message material")
	}
}

func TestEnqueueBackpressureDeletesRejectedRecord(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 1)
	if _, _, err := queue.Enqueue(context.Background(), testMessage("a", "b", "one")); err != nil {
		t.Fatal(err)
	}
	second := testMessage("c", "d", "two")
	if _, _, err := queue.Enqueue(context.Background(), second); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("second enqueue error = %v, want ErrBackpressure", err)
	}
	secret := &corev1.Secret{}
	err := queue.Client.Get(context.Background(), types.NamespacedName{Namespace: queue.Namespace, Name: recordName(second.DedupeKey)}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("rejected record still exists: %v", err)
	}
}

func TestConcurrentIdenticalEnqueueCreatesOneRecord(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 100)
	message := testMessage("a", "b", "summary")
	const producers = 20
	var wait sync.WaitGroup
	wait.Add(producers)
	results := make(chan bool, producers)
	errorsChannel := make(chan error, producers)
	for range producers {
		go func() {
			defer wait.Done()
			_, created, err := queue.Enqueue(context.Background(), message)
			results <- created
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	created := 0
	for result := range results {
		if result {
			created++
		}
	}
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
}

func TestPostCreateAPIFailureLeavesUnclaimableResumableAdmission(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 10)
	baseClient := queue.Client
	injected := errors.New("injected API outage")
	failureClient := &queueFailureClient{Client: baseClient, listErr: injected, deleteErr: injected}
	queue.Client = failureClient
	message := testMessage("a", "b", "protected summary")
	if _, created, err := queue.Enqueue(context.Background(), message); err == nil || created {
		t.Fatalf("uncertain enqueue = (created=%t, error=%v)", created, err)
	}
	pending, err := queue.getAdmission(context.Background(), message.DedupeKey)
	if err != nil || !pending.AdmissionPending || pending.ExpiresAt.IsZero() || pending.Revision != 1 {
		t.Fatalf("admission pending=%+v error=%v", pending, err)
	}
	failureClient.listErr = nil
	if _, err := queue.Claim(context.Background(), testHash("c"), time.Minute); !errors.Is(err, ErrNoClaim) {
		t.Fatalf("pre-admission record became claimable: %v", err)
	}
	queue.Client = baseClient
	resumed, created, err := queue.Enqueue(context.Background(), message)
	if err != nil || !created || resumed.AdmissionPending || !resumed.ExpiresAt.IsZero() || resumed.Revision != 2 {
		t.Fatalf("resumed admission = (%+v, created=%t, error=%v)", resumed, created, err)
	}
	claimed, err := queue.Claim(context.Background(), testHash("c"), time.Minute)
	if err != nil || claimed.DedupeKey != message.DedupeKey {
		t.Fatalf("admitted claim=%+v error=%v", claimed, err)
	}
}

func TestCapacityListTimeoutLeavesUnclaimableResumableAdmission(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 10)
	baseClient := queue.Client
	queue.Client = &queueFailureClient{Client: baseClient, listUntilContextDone: true, rejectCanceledDelete: true}
	message := testMessage("b", "c", "protected summary")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, created, err := queue.Enqueue(ctx, message); !errors.Is(err, context.DeadlineExceeded) || created {
		t.Fatalf("timed-out enqueue = (created=%t, error=%v)", created, err)
	}
	pending, err := queue.getAdmission(context.Background(), message.DedupeKey)
	if err != nil || !pending.AdmissionPending || pending.Revision != 1 {
		t.Fatalf("timed-out admission = (%+v, %v)", pending, err)
	}
	queue.Client = baseClient
	if _, err := queue.Claim(context.Background(), testHash("d"), time.Minute); !errors.Is(err, ErrNoClaim) {
		t.Fatalf("timed-out candidate became claimable: %v", err)
	}
	resumed, created, err := queue.Enqueue(context.Background(), message)
	if err != nil || !created || resumed.AdmissionPending || resumed.Revision != 2 {
		t.Fatalf("resumed timed-out admission = (%+v, created=%t, error=%v)", resumed, created, err)
	}
}

func TestFinalCreateLostResponseIsIdempotentlyRecovered(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 10)
	baseClient := queue.Client
	queue.Client = &queueFailureClient{Client: baseClient, finalCreateAfterApplyErr: errors.New("lost create response")}
	message := testMessage("d", "e", "protected summary")
	recovered, created, err := queue.Enqueue(context.Background(), message)
	if err != nil || created || recovered.AdmissionPending || recovered.Revision != 2 {
		t.Fatalf("lost-response recovery = (%+v, created=%t, error=%v)", recovered, created, err)
	}
	queue.Client = baseClient
	recovered, created, err = queue.Enqueue(context.Background(), message)
	if err != nil || created || recovered.AdmissionPending || recovered.Revision != 2 {
		t.Fatalf("idempotent replay = (%+v, created=%t, error=%v)", recovered, created, err)
	}
	admission := &corev1.Secret{}
	if err := baseClient.Get(context.Background(), types.NamespacedName{Namespace: queue.Namespace, Name: admissionName(message.DedupeKey)}, admission); !apierrors.IsNotFound(err) {
		t.Fatalf("candidate remains after recovered final create: %v", err)
	}
}

func TestAdmissionCandidateIsImmutableAndRejectedAsFinalRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	record := Record{
		Version: recordVersion, Revision: 1, DedupeKey: testHash("a"), MessageHash: testHash("b"),
		RouteKey: testHash("c"), Text: "protected summary", State: StatePending,
		AdmissionPending: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(admissionTimeout),
	}
	secret, err := encodeAdmission("delivery", record)
	if err != nil {
		t.Fatal(err)
	}
	if secret.Name != admissionName(record.DedupeKey) || secret.Immutable == nil || !*secret.Immutable ||
		secret.Labels[recordTypeLabel] != recordTypeAdmission || len(secret.Labels) != 1 || len(secret.Data) != 1 {
		t.Fatalf("admission secret = %#v", secret)
	}
	decoded, err := decodeAdmission(secret, record.DedupeKey)
	if err != nil || !decoded.AdmissionPending || decoded.Revision != 1 {
		t.Fatalf("decoded admission = (%+v, %v)", decoded, err)
	}
	if _, err := decode(secret, record.DedupeKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("candidate decoded as final record: %v", err)
	}
	if err := ValidateAdmissionPayload(secret.Name, secret.Data[admissionDataKey]); err != nil {
		t.Fatalf("validate admission payload: %v", err)
	}
	if err := ValidateRecordPayload(recordName(record.DedupeKey), string(StatePending), secret.Data[admissionDataKey]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("candidate validated as final payload: %v", err)
	}
	record.Revision = 2
	if _, err := encodeAdmission("delivery", record); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revision-2 candidate accepted: %v", err)
	}
}

func TestSweepRemovesOnlyExpiredAbandonedAdmission(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	baseClient := queue.Client
	injected := errors.New("injected API outage")
	queue.Client = &queueFailureClient{Client: baseClient, listErr: injected, deleteErr: injected}
	message := testMessage("f", "a", "protected summary")
	if _, _, err := queue.Enqueue(context.Background(), message); err == nil {
		t.Fatal("injected admission failure succeeded")
	}
	queue.Client = baseClient
	if removed, err := queue.Sweep(context.Background(), 10); err != nil || removed != 0 {
		t.Fatalf("early admission sweep = (%d, %v)", removed, err)
	}
	*now = now.Add(admissionTimeout)
	if removed, err := queue.Sweep(context.Background(), 10); err != nil || removed != 1 {
		t.Fatalf("expired admission sweep = (%d, %v)", removed, err)
	}
	secret := &corev1.Secret{}
	if err := baseClient.Get(context.Background(), types.NamespacedName{Namespace: queue.Namespace, Name: admissionName(message.DedupeKey)}, secret); !apierrors.IsNotFound(err) {
		t.Fatalf("expired pre-admission record remains: %v", err)
	}
}

func TestRetryBecomesClaimableAndThenDeadLetters(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	queue.MaxAttempts = 2
	message := testMessage("a", "b", "summary")
	if _, _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	owner := testHash("c")
	claim, err := queue.Claim(context.Background(), owner, time.Minute)
	if err != nil || claim.State != StateInFlight {
		t.Fatalf("claim = (%+v, %v)", claim, err)
	}
	retry, err := queue.Fail(context.Background(), claim, FailureRateLimited, 30*time.Second, false)
	if err != nil || retry.State != StateRetry || retry.Attempts != 1 {
		t.Fatalf("retry = (%+v, %v)", retry, err)
	}
	if _, err := queue.Claim(context.Background(), owner, time.Minute); !errors.Is(err, ErrNoClaim) {
		t.Fatalf("early claim error = %v, want ErrNoClaim", err)
	}
	*now = now.Add(30 * time.Second)
	claim, err = queue.Claim(context.Background(), owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dlq, err := queue.Fail(context.Background(), claim, FailureUnavailable, time.Second, false)
	if err != nil || dlq.State != StateDeadLetter || dlq.Attempts != 2 || dlq.Text != message.Text || dlq.ExpiresAt.IsZero() {
		t.Fatalf("DLQ = (%+v, %v)", dlq, err)
	}
	*now = dlq.ExpiresAt
	removed, err := queue.Sweep(context.Background(), 10)
	if err != nil || removed != 1 {
		t.Fatalf("sweep = (%d, %v)", removed, err)
	}
}

func TestCompleteCreatesPayloadFreeDedupeTombstone(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	message := testMessage("a", "b", "summary")
	if _, _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim(context.Background(), testHash("c"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := queue.Complete(context.Background(), claim)
	if err != nil || sent.State != StateSent || sent.Text != "" || sent.RouteKey != "" || sent.MessageHash == "" {
		t.Fatalf("complete = (%+v, %v)", sent, err)
	}
	if existing, created, err := queue.Enqueue(context.Background(), message); err != nil || created || existing.State != StateSent {
		t.Fatalf("dedupe after success = (%+v, %t, %v)", existing, created, err)
	}
	*now = sent.ExpiresAt
	if removed, err := queue.Sweep(context.Background(), 1); err != nil || removed != 1 {
		t.Fatalf("sweep = (%d, %v)", removed, err)
	}
}

func TestSweepAuditsRetentionWithoutProtectedData(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	recorder := &queueAuditRecorder{}
	queue.Audit = recorder
	message := testMessage("a", "b", "payload-SHOULD-NOT-LEAK")
	if _, _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim(context.Background(), testHash("c"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := queue.Complete(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	*now = sent.ExpiresAt
	if removed, err := queue.Sweep(context.Background(), 1); err != nil || removed != 1 {
		t.Fatalf("sweep = (%d, %v)", removed, err)
	}
	if len(recorder.events) != 2 || recorder.events[0].Outcome != audit.OutcomeStarted || recorder.events[0].Reason != audit.ReasonRetentionExpired || recorder.events[1].Outcome != audit.OutcomeSucceeded || recorder.events[1].Reason != audit.ReasonMutationVerified {
		t.Fatalf("audit events = %+v", recorder.events)
	}
	for _, event := range recorder.events {
		if event.Component != audit.ComponentDelivery || event.Action != audit.ActionDelete || event.LocalResource != "delivery/queue-retention" || event.Revision != sent.Revision {
			t.Fatalf("audit event = %+v", event)
		}
		payload, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{message.DedupeKey, message.RouteKey, message.Text} {
			if strings.Contains(string(payload), forbidden) {
				t.Fatalf("audit payload exposes protected data: %s", payload)
			}
		}
	}
}

func TestSweepFailsClosedWhenRetentionAuditIsUnavailable(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	message := testMessage("a", "b", "protected summary")
	if _, _, err := queue.Enqueue(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim(context.Background(), testHash("c"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := queue.Complete(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	*now = sent.ExpiresAt
	queue.Audit = &queueAuditRecorder{err: errors.New("injected audit outage")}
	if removed, err := queue.Sweep(context.Background(), 1); removed != 0 || !errors.Is(err, ErrAuditUnavailable) {
		t.Fatalf("sweep = (%d, %v), want fail-closed audit error", removed, err)
	}
	secret := &corev1.Secret{}
	if err := queue.Client.Get(context.Background(), types.NamespacedName{Namespace: queue.Namespace, Name: recordName(message.DedupeKey)}, secret); err != nil {
		t.Fatalf("retained queue record = %v", err)
	}
}

func TestExpiredLeaseCanBeReclaimedAndStaleCompletionFails(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	if _, _, err := queue.Enqueue(context.Background(), testMessage("a", "b", "summary")); err != nil {
		t.Fatal(err)
	}
	first, err := queue.Claim(context.Background(), testHash("c"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := queue.Complete(context.Background(), first); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired completion error = %v, want ErrClaimLost", err)
	}
	second, err := queue.Claim(context.Background(), testHash("d"), time.Minute)
	if err != nil || second.ClaimOwner == first.ClaimOwner || second.Revision != first.Revision+1 {
		t.Fatalf("reclaim = (%+v, %v)", second, err)
	}
	if _, err := queue.Complete(context.Background(), first); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale completion error = %v, want ErrClaimLost", err)
	}
	if _, err := queue.Complete(context.Background(), second); err != nil {
		t.Fatal(err)
	}
}

func TestQueueRejectsOversizeAndNonCanonicalKeys(t *testing.T) {
	t.Parallel()
	queue, _ := newTestQueue(t, 10)
	oversize := testMessage("a", "b", strings.Repeat("x", maxMessageRunes+1))
	if _, _, err := queue.Enqueue(context.Background(), oversize); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize error = %v", err)
	}
	nonCanonical := testMessage("a", "b", "summary")
	nonCanonical.DedupeKey = strings.Repeat("A", 64)
	if _, _, err := queue.Enqueue(context.Background(), nonCanonical); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-canonical key error = %v", err)
	}
}

func TestQueueStatisticsExposeOnlyAggregateStateAndAge(t *testing.T) {
	t.Parallel()
	queue, now := newTestQueue(t, 10)
	first, _, err := queue.Enqueue(context.Background(), testMessage("a", "b", "payload-SHOULD-NOT-LEAK"))
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	second, _, err := queue.Enqueue(context.Background(), testMessage("c", "d", "another-payload-SHOULD-NOT-LEAK"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim(context.Background(), testHash("e"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.DedupeKey != first.DedupeKey || second.State != StatePending {
		t.Fatalf("claimed = %+v, second = %+v", claimed, second)
	}
	statistics, err := queue.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if statistics.ByState[StateInFlight] != 1 || statistics.ByState[StatePending] != 1 || statistics.OldestOutstandingAge != 2*time.Minute {
		t.Fatalf("statistics = %+v", statistics)
	}
}

func newTestQueue(t *testing.T, capacity int) (*Queue, *time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	queue := &Queue{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state",
		MaxOutstanding: capacity, MaxAttempts: 3,
		DedupeRetention: time.Hour, DLQRetention: 24 * time.Hour,
		Audit: &queueAuditRecorder{},
	}
	queue.Now = func() time.Time { return now }
	return queue, &now
}

type queueAuditRecorder struct {
	events []audit.Event
	err    error
}

func (r *queueAuditRecorder) Record(_ context.Context, event audit.Event) error {
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, event)
	return nil
}

func testMessage(dedupe, route, text string) Message {
	return Message{DedupeKey: testHash(dedupe), RouteKey: testHash(route), Text: text}
}

func testHash(character string) string {
	return strings.Repeat(strings.ToLower(character), 64)
}

type queueFailureClient struct {
	client.Client
	listErr                  error
	deleteErr                error
	finalCreateAfterApplyErr error
	listUntilContextDone     bool
	rejectCanceledDelete     bool
}

func (c *queueFailureClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	secret, ok := object.(*corev1.Secret)
	if !ok || secret.Labels[recordTypeLabel] != recordTypeQueue || c.finalCreateAfterApplyErr == nil {
		return c.Client.Create(ctx, object, options...)
	}
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	return c.finalCreateAfterApplyErr
}

func (c *queueFailureClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if c.listUntilContextDone {
		<-ctx.Done()
		return ctx.Err()
	}
	if c.listErr != nil {
		return c.listErr
	}
	return c.Client.List(ctx, list, options...)
}

func (c *queueFailureClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if c.rejectCanceledDelete && ctx.Err() != nil {
		return ctx.Err()
	}
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.Client.Delete(ctx, object, options...)
}
