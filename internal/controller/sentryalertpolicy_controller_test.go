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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
)

func TestPolicyConsumerRolloutRequiresPolicyChildrenToBeReady(t *testing.T) {
	t.Parallel()

	policy := &sentryv1alpha1.SentryAlertPolicy{Spec: sentryv1alpha1.SentryAlertPolicySpec{
		NotificationTargets: &sentryv1alpha1.NotificationTargets{PagerDuty: map[string][]string{"company": {"Backend Primary"}}},
		Alerts:              map[string]sentryv1alpha1.AlertSpec{"volume": {Trigger: sentryv1alpha1.AlertTriggerEventCount}},
	}}
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: sentryv1alpha1.SentryProjectStatus{
			Conditions: []metav1.Condition{{Type: "AlertPolicyReady", Status: metav1.ConditionTrue, ObservedGeneration: 2}},
			NotificationTargets: map[string]sentryv1alpha1.DestinationStatus{
				"pagerDuty/Backend Primary": {Type: "pagerduty", State: destination.StateActive},
			},
			Alerts: map[string]sentryv1alpha1.AlertStatus{"volume": {State: alerts.StateReady}},
		},
	}
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionTrue {
		t.Fatalf("rollout status = %s, want True", got)
	}
	project.Status.Alerts["volume"] = sentryv1alpha1.AlertStatus{State: alerts.StateError}
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionFalse {
		t.Fatalf("failed child rollout status = %s, want False", got)
	}
	delete(project.Status.Alerts, "volume")
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionUnknown {
		t.Fatalf("missing child rollout status = %s, want Unknown", got)
	}
}

func TestProjectDriftReconcileIntervalRemainsPrompt(t *testing.T) {
	t.Parallel()
	if projectDriftReconcileInterval < 10*time.Second || projectDriftReconcileInterval > time.Minute {
		t.Fatalf("project drift reconcile interval = %s, want a bounded interval from 10s through 1m", projectDriftReconcileInterval)
	}
}

func TestAlertPolicyPendingRolloutIsRequeued(t *testing.T) {
	t.Parallel()

	result := alertPolicyRolloutResult(1)
	if result.RequeueAfter != policyRolloutRequeueInterval {
		t.Fatalf("pending rollout requeue = %s, want %s", result.RequeueAfter, policyRolloutRequeueInterval)
	}
	if result := alertPolicyRolloutResult(0); result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("complete rollout unexpectedly requeued: %#v", result)
	}
}
