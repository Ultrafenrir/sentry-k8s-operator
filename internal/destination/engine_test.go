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

package destination

import (
	"context"
	"errors"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/policy"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/resolver"
)

func TestPolicyDenialCreatesRevokeStateBeforeCatalogLookup(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	connection.Integrations = nil
	catalog := &fakeCatalog{}
	current := map[string]binding.DestinationBinding{
		"slack/incidents": {Kind: "slack", IntegrationIdentity: "workspace-id", ResourceIdentity: "channel-id", State: StateActive},
	}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: current})
	if !result.RevokeOnly || !policy.IsKind(result.PolicyErr, policy.ErrorDestinationUnavailable) || catalog.calls != 0 {
		t.Fatalf("result=%#v catalog calls=%d", result, catalog.calls)
	}
	if result.Bindings["slack/incidents"].State != StateRevoked || result.Bindings["slack/incidents"].ResourceIdentity != "channel-id" {
		t.Fatalf("revoked binding=%#v", result.Bindings["slack/incidents"])
	}
}

func TestResolvePinsReadableSlackAndPagerDutyIdentities(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{
			"slack":     {{Identity: "slack-workspace-id", Name: "company"}},
			"pagerduty": {{Identity: "pd-account-id", Name: "primary"}},
		},
		slack:     map[string][]Resource{"slack-workspace-id": {{Identity: "channel-id", Name: "#incidents"}}},
		pagerDuty: map[string][]Resource{"pd-account-id": {{Identity: "service-id", Name: "checkout"}}},
	}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Bindings["slack/incidents"].IntegrationIdentity != "slack-workspace-id" || result.Bindings["slack/incidents"].ResourceIdentity != "channel-id" || result.Bindings["pagerDuty/checkout"].ResourceIdentity != "service-id" {
		t.Fatalf("bindings=%#v", result.Bindings)
	}
	if result.Statuses["slack/incidents"].State != StateActive || result.Statuses["slack/incidents"].Reason != "DestinationResolved" {
		t.Fatalf("safe status=%#v", result.Statuses["slack/incidents"])
	}
}

func TestOneDestinationGrantRevokeDoesNotBlockAllowedSibling(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	connection.Integrations.Slack = nil
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{"pagerduty": {{Identity: "pd-account-id", Name: "primary"}}},
		pagerDuty:    map[string][]Resource{"pd-account-id": {{Identity: "service-id", Name: "checkout"}}},
	}
	current := map[string]binding.DestinationBinding{
		"slack/incidents": {
			Kind: "slack", SelectorHash: "old-selector", IntegrationIdentity: "workspace-id",
			IntegrationName: "company", ResourceIdentity: "channel-id", ResourceName: "incidents", State: StateActive,
		},
	}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{
		Namespace: "payments", Project: project, Connection: connection, Current: current,
	})
	if result.RevokeOnly || result.PolicyErr != nil {
		t.Fatalf("unexpected global revoke=%#v", result)
	}
	if !policy.IsKind(result.Statuses["slack/incidents"].Err, policy.ErrorDestinationUnavailable) || result.Bindings["slack/incidents"].State != StateRevoked || result.Bindings["slack/incidents"].ResourceIdentity != "channel-id" {
		t.Fatalf("revoked sibling=%#v status=%#v", result.Bindings["slack/incidents"], result.Statuses["slack/incidents"])
	}
	if result.Bindings["pagerDuty/checkout"].State != StateActive || result.Bindings["pagerDuty/checkout"].ResourceIdentity != "service-id" || catalog.calls != 2 {
		t.Fatalf("allowed sibling=%#v catalog calls=%d", result.Bindings["pagerDuty/checkout"], catalog.calls)
	}
}

func TestPinnedDestinationNeverSwitchesToSameNameReplacement(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.NotificationTargets.PagerDuty = nil
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{"slack": {{Identity: "workspace-old", Name: "company"}}},
		slack:        map[string][]Resource{"workspace-old": {{Identity: "channel-old", Name: "incidents"}}},
	}
	engine := &Engine{Catalog: catalog}
	first := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	if err := first.Err(); err != nil {
		t.Fatal(err)
	}
	catalog.integrations["slack"] = []Integration{{Identity: "workspace-new", Name: "company"}}
	catalog.slack = map[string][]Resource{"workspace-new": {{Identity: "channel-new", Name: "incidents"}}}
	second := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: first.Bindings})
	if !errors.Is(second.Statuses["slack/incidents"].Err, ErrExternalObjectReplaced) {
		t.Fatalf("replacement status=%#v", second.Statuses["slack/incidents"])
	}
	if second.Bindings["slack/incidents"].IntegrationIdentity != "workspace-old" || second.Bindings["slack/incidents"].ResourceIdentity != "channel-old" {
		t.Fatalf("pin switched: %#v", second.Bindings["slack/incidents"])
	}
}

func TestPinnedDestinationReportsExternalRename(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.NotificationTargets.PagerDuty = nil
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{"slack": {{Identity: "workspace-id", Name: "company"}}},
		slack:        map[string][]Resource{"workspace-id": {{Identity: "channel-id", Name: "incidents"}}},
	}
	engine := &Engine{Catalog: catalog}
	first := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	catalog.slack["workspace-id"] = []Resource{{Identity: "channel-id", Name: "renamed"}}
	second := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: first.Bindings})
	if !errors.Is(second.Statuses["slack/incidents"].Err, ErrExternalNameChanged) {
		t.Fatalf("rename status=%#v", second.Statuses["slack/incidents"])
	}
}

func TestProviderDestinationRemovalKeepsPinnedIdentityUntilAlertCleanup(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.NotificationTargets = &sentryv1alpha1.NotificationTargets{}
	current := map[string]binding.DestinationBinding{
		"slack/incidents": {
			Kind: "slack", SelectorHash: "selector", IntegrationIdentity: "workspace-id",
			ResourceIdentity: "channel-id", ResourceName: "incidents", State: StateActive,
		},
		"pagerDuty/checkout": {
			Kind: "pagerduty", SelectorHash: "selector", IntegrationIdentity: "account-id",
			ResourceIdentity: "service-id", ResourceName: "checkout", State: StateActive,
		},
	}
	result := (&Engine{}).Reconcile(context.Background(), Input{
		Namespace: "payments", Project: project, Connection: connection, Current: current,
	})
	for _, name := range []string{"slack/incidents", "pagerDuty/checkout"} {
		if result.Bindings[name].State != StateCleanupPending || result.Bindings[name].ResourceIdentity != current[name].ResourceIdentity {
			t.Fatalf("%s cleanup binding=%#v", name, result.Bindings[name])
		}
		if result.Statuses[name].Reason != "DestinationRemoved" {
			t.Fatalf("%s cleanup status=%#v", name, result.Statuses[name])
		}
	}
}

func TestDuplicateReadableNamesRemainAmbiguous(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.NotificationTargets.PagerDuty = nil
	catalog := &fakeCatalog{integrations: map[string][]Integration{"slack": {
		{Identity: "one", Name: "company"}, {Identity: "two", Name: "company"},
	}}}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	if !resolver.IsKind(result.Statuses["slack/incidents"].Err, resolver.ErrorAmbiguous) {
		t.Fatalf("status=%#v", result.Statuses["slack/incidents"])
	}
}

func destinationInput() (sentryv1alpha1.SentryProjectSpec, sentryv1alpha1.SentryConnectionSpec) {
	project := sentryv1alpha1.SentryProjectSpec{
		Team:                "payments",
		NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}, PagerDuty: map[string][]string{"primary": {"checkout"}}},
	}
	connection := sentryv1alpha1.SentryConnectionSpec{
		Organization: "company",
		Integrations: &sentryv1alpha1.ConnectionIntegrations{
			Slack:     &sentryv1alpha1.SlackIntegration{Workspaces: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "company"}}},
			PagerDuty: &sentryv1alpha1.PagerDutyIntegration{Accounts: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "primary", SentryName: "primary"}}},
		},
	}
	return project, connection
}

type fakeCatalog struct {
	calls        int
	integrations map[string][]Integration
	slack        map[string][]Resource
	pagerDuty    map[string][]Resource
}

func (c *fakeCatalog) ListIntegrations(_ context.Context, _, provider string) ([]Integration, error) {
	c.calls++
	return append([]Integration(nil), c.integrations[provider]...), nil
}

func (c *fakeCatalog) ListSlackChannels(_ context.Context, _, integration string) ([]Resource, error) {
	c.calls++
	return append([]Resource(nil), c.slack[integration]...), nil
}

func (c *fakeCatalog) ListPagerDutyServices(_ context.Context, _, integration string) ([]Resource, error) {
	c.calls++
	return append([]Resource(nil), c.pagerDuty[integration]...), nil
}
