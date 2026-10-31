package alertpolicy

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestMergeKeepsPolicyAndProjectOwnershipScopesSeparate(t *testing.T) {
	policy := &sentryv1alpha1.SentryAlertPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", UID: types.UID("policy-uid"), Generation: 4},
		Spec: sentryv1alpha1.SentryAlertPolicySpec{
			Sentry: "company", NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"company-slack": {"backend-alerts"}}},
			Alerts: map[string]sentryv1alpha1.AlertSpec{"volume": {Trigger: sentryv1alpha1.AlertTriggerEventCount, Notify: []string{"slack/backend-alerts"}}},
		},
	}
	localAlerts := map[string]sentryv1alpha1.AlertSpec{"regression": {Trigger: sentryv1alpha1.AlertTriggerRegression, Notify: []string{"pagerDuty/Backend Primary"}}}
	project := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Name: "checkout", UID: types.UID("project-uid"), Generation: 2}, Spec: sentryv1alpha1.SentryProjectSpec{
		Sentry: "company", NotificationTargets: &sentryv1alpha1.NotificationTargets{PagerDuty: map[string][]string{"company-pagerduty": {"Backend Primary"}}}, Alerts: &localAlerts,
	}}
	result, err := Merge(project, policy)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.PolicyUID != "policy-uid" || len(*result.Alerts) != 2 {
		t.Fatalf("result=%#v", result)
	}
}

func TestMergeRejectsSameProviderReadableNameConflict(t *testing.T) {
	policy := &sentryv1alpha1.SentryAlertPolicy{Spec: sentryv1alpha1.SentryAlertPolicySpec{Sentry: "company", NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"one": {"incidents"}}}}}
	project := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Name: "checkout", UID: types.UID("project-uid"), Generation: 2}, Spec: sentryv1alpha1.SentryProjectSpec{Sentry: "company", NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"two": {"incidents"}}}}}
	if _, err := Merge(project, policy); !errors.Is(err, ErrPolicyLocalConflict) {
		t.Fatalf("error=%v", err)
	}
}

func TestMergeRejectsCrossOwnershipTargetReference(t *testing.T) {
	policyAlerts := map[string]sentryv1alpha1.AlertSpec{"policy-alert": {Trigger: sentryv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/local"}}}
	policy := &sentryv1alpha1.SentryAlertPolicy{Spec: sentryv1alpha1.SentryAlertPolicySpec{Sentry: "company", Alerts: policyAlerts}}
	project := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Name: "checkout", UID: types.UID("project-uid"), Generation: 2}, Spec: sentryv1alpha1.SentryProjectSpec{Sentry: "company", NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"local"}}}}}
	if _, err := Merge(project, policy); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("error=%v", err)
	}
}
