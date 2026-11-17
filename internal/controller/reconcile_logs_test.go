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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestConditionLogSummaryIsDeterministicAndBounded(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ProjectVerified", Message: "must not be logged"},
		{Type: "AlertsReady", Status: metav1.ConditionFalse, Reason: "AlertFailed", Message: "must not be logged"},
		{Type: "OwnershipReady", Status: metav1.ConditionUnknown, Reason: "NotConfigured", Message: "must not be logged"},
	}
	want := []string{"AlertsReady=False/AlertFailed", "Ready=True/ProjectVerified"}
	if got := conditionLogSummary(conditions); !reflect.DeepEqual(got, want) {
		t.Fatalf("conditionLogSummary() = %#v, want %#v", got, want)
	}
}

func TestProjectStateCountsExposeOnlyAggregateStates(t *testing.T) {
	statuses := map[string]sentryv1alpha1.AlertStatus{
		"first":  {State: "Ready", Reason: "AlertReady"},
		"second": {State: "Ready", Reason: "AlertReady"},
		"third":  {State: "Error", Reason: "AlertFailed"},
	}
	want := map[string]int{"Ready": 2, "Error": 1}
	if got := projectAlertStateCounts(statuses); !reflect.DeepEqual(got, want) {
		t.Fatalf("projectAlertStateCounts() = %#v, want %#v", got, want)
	}
}
