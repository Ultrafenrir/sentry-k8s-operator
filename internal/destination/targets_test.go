package destination

import (
	"errors"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestFlattenTargetsUsesReadableProviderReferences(t *testing.T) {
	targets := &sentryv1alpha1.NotificationTargets{
		Slack:     map[string][]string{"company-slack": {"backend-alerts", "security-alerts"}},
		PagerDuty: map[string][]string{"company-pagerduty": {"Backend Primary"}},
	}
	got, err := FlattenTargets(targets)
	if err != nil {
		t.Fatalf("FlattenTargets: %v", err)
	}
	if got["slack/backend-alerts"].Slack.Workspace != "company-slack" || got["pagerDuty/Backend Primary"].PagerDuty.Account != "company-pagerduty" {
		t.Fatalf("flattened targets=%#v", got)
	}
}

func TestFlattenTargetsRejectsSameChannelNameAcrossWorkspaces(t *testing.T) {
	targets := &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{
		"company-slack":  {"incidents"},
		"acquired-slack": {"incidents"},
	}}
	if _, err := FlattenTargets(targets); !errors.Is(err, ErrAmbiguousTargetName) {
		t.Fatalf("error=%v", err)
	}
}
