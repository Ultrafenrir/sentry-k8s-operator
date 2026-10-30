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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

type staticStatistics struct {
	value delivery.Statistics
	err   error
}

func (s staticStatistics) Statistics(context.Context) (delivery.Statistics, error) {
	return s.value, s.err
}

func TestMetricsExposeOnlyFixedLabelsAndAggregateQueueState(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.Observe(Result{Outcome: OutcomeRetry, Failure: delivery.FailureRateLimited, Duration: 1500 * time.Millisecond})
	if err := metrics.Refresh(context.Background(), staticStatistics{value: delivery.Statistics{
		ByState:              map[delivery.State]int{delivery.StateRetry: 2, delivery.StateDeadLetter: 1},
		OldestOutstandingAge: 3 * time.Minute,
	}}); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	encoded := metricText(families)
	for _, forbidden := range []string{"chat-SHOULD-NOT-LEAK", "route-SHOULD-NOT-LEAK", "payload-SHOULD-NOT-LEAK"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("metrics leaked %q: %s", forbidden, encoded)
		}
	}
	for _, expected := range []string{"outcome=retry", "failure=RateLimited", "state=Retry", "state=DeadLetter"} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("metrics missing %q: %s", expected, encoded)
		}
	}
}

func TestMetricsClampUnknownEnumsAndCountRefreshErrors(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.Observe(Result{Outcome: "private-outcome-SHOULD-NOT-LEAK", Failure: "private-error-SHOULD-NOT-LEAK", Duration: -time.Second})
	if err := metrics.Refresh(context.Background(), staticStatistics{err: errors.New("private-refresh-error-SHOULD-NOT-LEAK")}); err == nil {
		t.Fatal("expected refresh error")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	encoded := metricText(families)
	if strings.Contains(encoded, "SHOULD-NOT-LEAK") || !strings.Contains(encoded, "outcome=error") || !strings.Contains(encoded, "failure=unknown") {
		t.Fatalf("unsafe metrics: %s", encoded)
	}
}

func metricText(families []*dto.MetricFamily) string {
	var builder strings.Builder
	for _, family := range families {
		builder.WriteString(family.GetName())
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				builder.WriteString(" ")
				builder.WriteString(label.GetName())
				builder.WriteString("=")
				builder.WriteString(label.GetValue())
			}
		}
	}
	return builder.String()
}
