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
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

var ErrMetrics = errors.New("delivery metrics initialization failed")

type StatisticsProvider interface {
	Statistics(context.Context) (delivery.Statistics, error)
}

// Metrics deliberately uses only closed enum labels. Route keys, record names,
// chat identities, project names, payloads and upstream error strings are never
// accepted as label values.
type Metrics struct {
	attempts      *prometheus.CounterVec
	duration      prometheus.Histogram
	queueDepth    *prometheus.GaugeVec
	oldestAge     prometheus.Gauge
	refreshErrors prometheus.Counter
}

func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	if registerer == nil {
		return nil, ErrMetrics
	}
	metrics := &Metrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sentry_operator_telegram_delivery_attempts_total",
			Help: "Settled Telegram delivery attempts by fixed outcome and failure class.",
		}, []string{"outcome", "failure"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sentry_operator_telegram_delivery_processing_seconds",
			Help:    "Time spent claiming, sending and durably settling one delivery item.",
			Buckets: prometheus.DefBuckets,
		}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sentry_operator_telegram_delivery_queue_records",
			Help: "Protected delivery queue records by fixed state.",
		}, []string{"state"}),
		oldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sentry_operator_telegram_delivery_oldest_outstanding_seconds",
			Help: "Age of the oldest non-sent protected delivery record.",
		}),
		refreshErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sentry_operator_telegram_delivery_metrics_refresh_errors_total",
			Help: "Failures while refreshing aggregate queue metrics.",
		}),
	}
	for _, collector := range []prometheus.Collector{metrics.attempts, metrics.duration, metrics.queueDepth, metrics.oldestAge, metrics.refreshErrors} {
		if err := registerer.Register(collector); err != nil {
			return nil, ErrMetrics
		}
	}
	for _, state := range deliveryStates() {
		metrics.queueDepth.WithLabelValues(state).Set(0)
	}
	return metrics, nil
}

func (m *Metrics) Observe(result Result) {
	if m == nil {
		return
	}
	m.attempts.WithLabelValues(safeOutcome(result.Outcome), safeFailure(result.Failure)).Inc()
	seconds := result.Duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.duration.Observe(seconds)
}

func (m *Metrics) Refresh(ctx context.Context, provider StatisticsProvider) error {
	if m == nil || provider == nil {
		return ErrMetrics
	}
	statistics, err := provider.Statistics(ctx)
	if err != nil {
		m.refreshErrors.Inc()
		return err
	}
	for _, state := range []delivery.State{delivery.StatePending, delivery.StateInFlight, delivery.StateRetry, delivery.StateSent, delivery.StateDeadLetter} {
		m.queueDepth.WithLabelValues(string(state)).Set(float64(statistics.ByState[state]))
	}
	m.oldestAge.Set(statistics.OldestOutstandingAge.Seconds())
	return nil
}

func safeOutcome(value Outcome) string {
	switch value {
	case OutcomeSent, OutcomeRetry, OutcomeDeadLetter, OutcomeError:
		return string(value)
	default:
		return "error"
	}
}

func safeFailure(value delivery.Failure) string {
	switch value {
	case "":
		return "none"
	case delivery.FailureTransport, delivery.FailureUnavailable, delivery.FailureRateLimited, delivery.FailureRejected, delivery.FailureInvalidRoute:
		return string(value)
	default:
		return "unknown"
	}
}

func deliveryStates() []string {
	return []string{
		string(delivery.StatePending), string(delivery.StateInFlight), string(delivery.StateRetry),
		string(delivery.StateSent), string(delivery.StateDeadLetter),
	}
}

var _ Observer = (*Metrics)(nil)
var _ StatisticsProvider = (*delivery.Queue)(nil)
