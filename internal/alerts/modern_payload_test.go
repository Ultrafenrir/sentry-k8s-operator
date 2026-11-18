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

package alerts

import "testing"

func TestApplicationMetricSourceSerializesBoundedTermAndEquation(t *testing.T) {
	t.Parallel()

	right := ApplicationMetricTermDesired{Name: "checkout.latency", Type: "distribution", Unit: "millisecond", Aggregation: "p95"}
	desired := Desired{Metric: &MetricDesired{Preset: "application-metric", ApplicationMetric: &ApplicationMetricExpressionDesired{
		Left:     ApplicationMetricTermDesired{Name: "checkout.requests", Type: "counter", Aggregation: "per-minute"},
		Operator: "divide", Right: &right,
	}}}
	dataset, aggregate, query, eventTypes, err := modernMetricSource(desired)
	if err != nil {
		t.Fatal(err)
	}
	if dataset != "events_analytics_platform" || aggregate != "equation|per_minute(value,checkout.requests,counter,none) / p95(value,checkout.latency,distribution,millisecond)" || query != "" || len(eventTypes) != 1 || eventTypes[0] != "trace_item_metric" {
		t.Fatalf("source = %q %q %q %#v", dataset, aggregate, query, eventTypes)
	}
}

func TestApplicationMetricSourceRejectsIncompleteExpression(t *testing.T) {
	t.Parallel()

	desired := Desired{Metric: &MetricDesired{Preset: "application-metric", ApplicationMetric: &ApplicationMetricExpressionDesired{
		Left: ApplicationMetricTermDesired{Name: "checkout.requests", Type: "counter", Aggregation: "sum"}, Operator: "add",
	}}}
	if _, _, _, _, err := modernMetricSource(desired); err == nil {
		t.Fatal("incomplete expression was accepted")
	}
}

func TestApplicationMetricSourceRejectsRawSyntax(t *testing.T) {
	t.Parallel()

	desired := Desired{Metric: &MetricDesired{Preset: "application-metric", ApplicationMetric: &ApplicationMetricExpressionDesired{
		Left: ApplicationMetricTermDesired{Name: "checkout.requests),count(value", Type: "counter", Aggregation: "sum"},
	}}}
	if _, _, _, _, err := modernMetricSource(desired); err == nil {
		t.Fatal("raw aggregate syntax was accepted")
	}
}
