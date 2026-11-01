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

package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestControllerMetricsExposeOnlyFixedLabels(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.ObserveReconcile(controllerProject, ctrl.Result{RequeueAfter: time.Second}, nil, 1500*time.Millisecond)
	metrics.ObserveReconcile(controllerConnection, ctrl.Result{}, errors.New("private-error-SHOULD-NOT-LEAK"), -time.Second)
	metrics.ObserveCondition(controllerProject, "Ready", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "AlertsReady", metav1.ConditionFalse)
	metrics.ObserveReconcile(controllerAlertPolicy, ctrl.Result{}, nil, time.Second)
	metrics.ObserveCondition(controllerAlertPolicy, "RolloutComplete", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "AlertPolicyReady", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "GrantAccessReady", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "IssueOwnershipReady", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "ClientKeysReady", metav1.ConditionTrue)
	metrics.ObserveCondition(controllerProject, "SymbolSourcesReady", metav1.ConditionTrue)
	metrics.ObserveCondition("private-controller-SHOULD-NOT-LEAK", "private-condition-SHOULD-NOT-LEAK", "private-status-SHOULD-NOT-LEAK")

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	encoded := controllerMetricText(families)
	if strings.Contains(encoded, "SHOULD-NOT-LEAK") {
		t.Fatalf("metrics leaked dynamic input: %s", encoded)
	}
	for _, expected := range []string{
		"controller=project outcome=requeue",
		"controller=connection outcome=error",
		"controller=alertpolicy outcome=success",
		"condition=Ready controller=project status=True",
		"condition=AlertsReady controller=project status=False",
		"condition=RolloutComplete controller=alertpolicy status=True",
		"condition=AlertPolicyReady controller=project status=True",
		"condition=GrantAccessReady controller=project status=True",
		"condition=IssueOwnershipReady controller=project status=True",
		"condition=ClientKeysReady controller=project status=True",
		"condition=SymbolSourcesReady controller=project status=True",
		"condition=unknown controller=unknown status=unknown",
	} {
		if !strings.Contains(encoded, expected) {
			t.Fatalf("metrics missing %q: %s", expected, encoded)
		}
	}
}

func TestSynchronizationMetricsAggregateWithoutResourceLabels(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.ObserveProjectSynchronization("private/team-a", []metav1.Condition{
		{Type: "ProjectReady", Status: metav1.ConditionTrue},
		{Type: "DSNReady", Status: metav1.ConditionFalse},
		{Type: "SettingsReady", Status: metav1.ConditionUnknown},
		{Type: "IssueOwnershipReady", Status: metav1.ConditionTrue},
		{Type: "ClientKeysReady", Status: metav1.ConditionTrue},
		{Type: "SymbolSourcesReady", Status: metav1.ConditionTrue},
	})
	metrics.ObserveProjectSynchronization("private/team-b", []metav1.Condition{
		{Type: "ProjectReady", Status: metav1.ConditionTrue},
		{Type: "DSNReady", Status: metav1.ConditionFalse},
		{Type: "AlertsReady", Status: metav1.ConditionFalse},
	})
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncDSN, syncBlocked), 2)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncAlerts, syncBlocked), 1)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncSettings, syncInactive), 1)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncIssueOwnership, syncReady), 1)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncSymbolSources, syncReady), 1)

	metrics.ObserveProjectSynchronization("private/team-a", []metav1.Condition{
		{Type: "ProjectReady", Status: metav1.ConditionTrue},
		{Type: "DSNReady", Status: metav1.ConditionTrue},
	})
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncDSN, syncBlocked), 1)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncDSN, syncReady), 1)
	metrics.ForgetProjectSynchronization("private/team-b")
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncDSN, syncBlocked), 0)
	assertGaugeValue(t, metrics.synchronizations.WithLabelValues(syncAlerts, syncBlocked), 0)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	encoded := controllerMetricText(families)
	if strings.Contains(encoded, "private") || strings.Contains(encoded, "team-a") || strings.Contains(encoded, "team-b") {
		t.Fatalf("metrics leaked an internal resource key: %s", encoded)
	}
}

func assertGaugeValue(t *testing.T, gauge prometheus.Gauge, expected float64) {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatal(err)
	}
	if metric.GetGauge().GetValue() != expected {
		t.Fatalf("gauge value = %v, want %v", metric.GetGauge().GetValue(), expected)
	}
}

func controllerMetricText(families []*dto.MetricFamily) string {
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
