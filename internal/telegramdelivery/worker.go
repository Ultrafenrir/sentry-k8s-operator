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
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

type QueueStore interface {
	Claim(context.Context, string, time.Duration) (delivery.Record, error)
	Complete(context.Context, delivery.Record) (delivery.Record, error)
	Fail(context.Context, delivery.Record, delivery.Failure, time.Duration, bool) (delivery.Record, error)
}

type RouteStore interface {
	GetTelegramRoute(context.Context, string) (binding.TelegramTarget, error)
}

type Sender interface {
	Send(context.Context, binding.TelegramTarget, string) error
}

type Outcome string

const (
	OutcomeSent       Outcome = "sent"
	OutcomeRetry      Outcome = "retry"
	OutcomeDeadLetter Outcome = "dead_letter"
	OutcomeError      Outcome = "error"
)

type Result struct {
	Outcome  Outcome
	Failure  delivery.Failure
	Duration time.Duration
}

type Observer interface {
	Observe(Result)
}

type Worker struct {
	Queue     QueueStore
	Routes    RouteStore
	Sender    Sender
	Owner     string
	Lease     time.Duration
	RetryBase time.Duration
	RetryMax  time.Duration
	Jitter    func(time.Duration) time.Duration
	Observer  Observer
	Audit     audit.Sink
}

var ErrAuditUnavailable = errors.New("Telegram delivery audit is unavailable")

// ProcessOne claims and durably settles at most one message. A successful
// Telegram call followed by a failed Complete can be delivered again after the
// lease expires; this is the explicit at-least-once boundary.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	started := time.Now()
	if !w.valid() {
		return false, delivery.ErrInvalid
	}
	record, err := w.Queue.Claim(ctx, w.Owner, w.Lease)
	if errors.Is(err, delivery.ErrNoClaim) {
		return false, nil
	}
	if err != nil {
		w.observe(started, OutcomeError, "")
		return false, err
	}
	target, err := w.Routes.GetTelegramRoute(ctx, record.RouteKey)
	if err != nil {
		if auditErr := w.recordAudit(ctx, record, audit.OutcomeFailed, audit.ReasonRouteUnavailable); auditErr != nil {
			w.observe(started, OutcomeError, "")
			return true, auditErr
		}
		permanent := apierrors.IsNotFound(err) || errors.Is(err, registration.ErrRouteInvalid) || errors.Is(err, registration.ErrRouteConflict)
		failure := delivery.FailureTransport
		if permanent {
			failure = delivery.FailureInvalidRoute
		}
		settled, settleErr := w.Queue.Fail(ctx, record, failure, w.retry(record.Attempts), permanent)
		w.observeSettlement(started, settled, settleErr, failure, permanent)
		return true, settleErr
	}
	if err := w.recordAudit(ctx, record, audit.OutcomeStarted, audit.ReasonDeliveryClaimed); err != nil {
		w.observe(started, OutcomeError, "")
		return true, err
	}
	if err := w.Sender.Send(ctx, target, record.Text); err != nil {
		if auditErr := w.recordAudit(ctx, record, audit.OutcomeFailed, audit.ReasonDeliveryFailed); auditErr != nil {
			w.observe(started, OutcomeError, "")
			return true, auditErr
		}
		failure := delivery.FailureTransport
		permanent := false
		retryAfter := time.Duration(0)
		if sendError, ok := AsSendError(err); ok {
			failure = sendError.Failure
			permanent = sendError.Permanent
			retryAfter = sendError.RetryAfter
		}
		backoff := w.retry(record.Attempts)
		if retryAfter < backoff {
			retryAfter = backoff
		}
		settled, settleErr := w.Queue.Fail(ctx, record, failure, retryAfter, permanent)
		w.observeSettlement(started, settled, settleErr, failure, permanent)
		return true, settleErr
	}
	if err := w.recordAudit(ctx, record, audit.OutcomeSucceeded, audit.ReasonDeliveryAccepted); err != nil {
		w.observe(started, OutcomeError, "")
		return true, err
	}
	_, err = w.Queue.Complete(ctx, record)
	if err != nil {
		w.observe(started, OutcomeError, "")
	} else {
		w.observe(started, OutcomeSent, "")
	}
	return true, err
}

func (w *Worker) recordAudit(ctx context.Context, record delivery.Record, outcome audit.Outcome, reason audit.Reason) error {
	event := audit.Event{
		Timestamp: time.Now().UTC().Round(0), Component: audit.ComponentDelivery,
		Action: audit.ActionDeliver, Outcome: outcome, Reason: reason,
		LocalResource: "delivery/queue", Revision: record.Revision,
	}
	if err := w.Audit.Record(ctx, event); err != nil {
		return ErrAuditUnavailable
	}
	return nil
}

func (w *Worker) observeSettlement(started time.Time, settled delivery.Record, settleErr error, failure delivery.Failure, permanent bool) {
	if settleErr != nil {
		w.observe(started, OutcomeError, failure)
		return
	}
	outcome := OutcomeRetry
	if permanent || settled.State == delivery.StateDeadLetter {
		outcome = OutcomeDeadLetter
	}
	w.observe(started, outcome, failure)
}

func (w *Worker) observe(started time.Time, outcome Outcome, failure delivery.Failure) {
	if w.Observer != nil {
		w.Observer.Observe(Result{Outcome: outcome, Failure: failure, Duration: time.Since(started)})
	}
}

func (w *Worker) retry(previousAttempts int) time.Duration {
	delay := w.RetryBase
	for attempt := 0; attempt < previousAttempts && delay < w.RetryMax; attempt++ {
		if delay > w.RetryMax/2 {
			delay = w.RetryMax
			break
		}
		delay *= 2
	}
	if delay > w.RetryMax {
		delay = w.RetryMax
	}
	if w.Jitter != nil {
		return boundedDuration(w.Jitter(delay), delay*9/10, minDuration(delay*11/10, w.RetryMax))
	}
	return boundedDuration(secureJitter(delay), delay*9/10, w.RetryMax)
}

func (w *Worker) valid() bool {
	return w != nil && w.Queue != nil && w.Routes != nil && w.Sender != nil && w.Audit != nil && validWorkerKey(w.Owner) && w.Lease > 0 && w.RetryBase > 0 && w.RetryMax >= w.RetryBase
}

func validWorkerKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func secureJitter(base time.Duration) time.Duration {
	// 2001 equally likely basis-point offsets cover [-10%, +10%]. Entropy
	// failure returns the base interval and never creates a tight loop.
	offset, err := rand.Int(rand.Reader, big.NewInt(2001))
	if err != nil {
		return base
	}
	factor := int64(9000 + offset.Int64())
	return time.Duration(int64(base) * factor / 10000)
}

func boundedDuration(value, minimum, maximum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

var _ QueueStore = (*delivery.Queue)(nil)
var _ RouteStore = (*registration.RouteStore)(nil)
var _ Sender = (*Client)(nil)
