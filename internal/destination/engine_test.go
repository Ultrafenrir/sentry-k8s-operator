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
	"strings"
	"testing"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/policy"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/resolver"
)

func TestPolicyDenialCreatesRevokeStateBeforeCatalogLookup(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	connection.Access[0].Destinations = nil
	catalog := &fakeCatalog{}
	current := map[string]binding.DestinationBinding{
		"ops": {Kind: "slack", IntegrationIdentity: "workspace-id", ResourceIdentity: "channel-id", State: StateActive},
	}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: current})
	if !result.RevokeOnly || !policy.IsKind(result.PolicyErr, policy.ErrorDestinationAccessDenied) || catalog.calls != 0 {
		t.Fatalf("result=%#v catalog calls=%d", result, catalog.calls)
	}
	if result.Bindings["ops"].State != StateRevoked || result.Bindings["ops"].ResourceIdentity != "channel-id" {
		t.Fatalf("revoked binding=%#v", result.Bindings["ops"])
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
	if result.Bindings["ops"].IntegrationIdentity != "slack-workspace-id" || result.Bindings["ops"].ResourceIdentity != "channel-id" || result.Bindings["oncall"].ResourceIdentity != "service-id" {
		t.Fatalf("bindings=%#v", result.Bindings)
	}
	if result.Statuses["ops"].State != StateActive || result.Statuses["ops"].Reason != "DestinationResolved" {
		t.Fatalf("safe status=%#v", result.Statuses["ops"])
	}
}

func TestOneDestinationGrantRevokeDoesNotBlockAllowedSibling(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	connection.Access[0].Destinations.Slack = nil
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{"pagerduty": {{Identity: "pd-account-id", Name: "primary"}}},
		pagerDuty:    map[string][]Resource{"pd-account-id": {{Identity: "service-id", Name: "checkout"}}},
	}
	current := map[string]binding.DestinationBinding{
		"ops": {
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
	if !policy.IsKind(result.Statuses["ops"].Err, policy.ErrorDestinationAccessDenied) || result.Bindings["ops"].State != StateRevoked || result.Bindings["ops"].ResourceIdentity != "channel-id" {
		t.Fatalf("revoked sibling=%#v status=%#v", result.Bindings["ops"], result.Statuses["ops"])
	}
	if result.Bindings["oncall"].State != StateActive || result.Bindings["oncall"].ResourceIdentity != "service-id" || catalog.calls != 2 {
		t.Fatalf("allowed sibling=%#v catalog calls=%d", result.Bindings["oncall"], catalog.calls)
	}
}

func TestPinnedDestinationNeverSwitchesToSameNameReplacement(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	delete(project.Destinations, "oncall")
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
	if !errors.Is(second.Statuses["ops"].Err, ErrExternalObjectReplaced) {
		t.Fatalf("replacement status=%#v", second.Statuses["ops"])
	}
	if second.Bindings["ops"].IntegrationIdentity != "workspace-old" || second.Bindings["ops"].ResourceIdentity != "channel-old" {
		t.Fatalf("pin switched: %#v", second.Bindings["ops"])
	}
}

func TestPinnedDestinationReportsExternalRename(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	delete(project.Destinations, "oncall")
	catalog := &fakeCatalog{
		integrations: map[string][]Integration{"slack": {{Identity: "workspace-id", Name: "company"}}},
		slack:        map[string][]Resource{"workspace-id": {{Identity: "channel-id", Name: "incidents"}}},
	}
	engine := &Engine{Catalog: catalog}
	first := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	catalog.slack["workspace-id"] = []Resource{{Identity: "channel-id", Name: "renamed"}}
	second := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: first.Bindings})
	if !errors.Is(second.Statuses["ops"].Err, ErrExternalNameChanged) {
		t.Fatalf("rename status=%#v", second.Statuses["ops"])
	}
}

func TestTelegramRequiresRegistrationAndRemovalKeepsCleanupTombstone(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.Destinations = map[string]sentryv1alpha1.DestinationSpec{
		"telegram": {Telegram: &sentryv1alpha1.TelegramDestination{Target: "payments-alerts"}},
	}
	connection.Access[0].Destinations = &sentryv1alpha1.DestinationAccess{Telegram: &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"payments-alerts"}}}
	catalog := &fakeCatalog{}
	engine := &Engine{Catalog: catalog}
	first := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	if first.Bindings["telegram"].State != StateRegistrationRequired || catalog.calls != 0 {
		t.Fatalf("first=%#v calls=%d", first, catalog.calls)
	}
	project.Destinations = map[string]sentryv1alpha1.DestinationSpec{}
	second := engine.Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection, Current: first.Bindings})
	if second.Bindings["telegram"].State != StateCleanupPending || second.Statuses["telegram"].Reason != "DestinationRemoved" {
		t.Fatalf("second=%#v", second)
	}
}

func TestTelegramSelectorChangeRequiresOldRouteRevocationBeforeNewRegistration(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.Destinations = map[string]sentryv1alpha1.DestinationSpec{
		"telegram": {Telegram: &sentryv1alpha1.TelegramDestination{Target: "new-target"}},
	}
	connection.Access[0].Destinations = &sentryv1alpha1.DestinationAccess{Telegram: &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"new-target"}}}
	old := binding.DestinationBinding{
		Kind: "telegram", SelectorHash: selectorHash("telegram", "", "old-target"), ResourceName: "old-target", State: StateActive,
		Telegram: &binding.TelegramBinding{Target: &binding.TelegramTarget{ChatIdentity: "-100123"}, RouteKey: strings.Repeat("a", 64)},
	}
	engine := &Engine{}
	cleanup := engine.Reconcile(context.Background(), Input{
		Namespace: "payments", Project: project, Connection: connection,
		Current: map[string]binding.DestinationBinding{"telegram": old},
	})
	if cleanup.Bindings["telegram"].State != StateCleanupPending || cleanup.Bindings["telegram"].Reason != "DestinationChanged" || cleanup.Bindings["telegram"].Telegram.Target.ChatIdentity != "-100123" {
		t.Fatalf("cleanup=%#v", cleanup.Bindings["telegram"])
	}
	revoked := cleanup.Bindings["telegram"]
	revoked.Telegram.RouteRevoked = true
	revertedProject := project
	revertedProject.Destinations = map[string]sentryv1alpha1.DestinationSpec{
		"telegram": {Telegram: &sentryv1alpha1.TelegramDestination{Target: "old-target"}},
	}
	revertedConnection := connection
	revertedConnection.Access = append([]sentryv1alpha1.AccessRule(nil), connection.Access...)
	revertedConnection.Access[0].Destinations = &sentryv1alpha1.DestinationAccess{Telegram: &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"old-target"}}}
	restored := engine.Reconcile(context.Background(), Input{
		Namespace: "payments", Project: revertedProject, Connection: revertedConnection,
		Current: map[string]binding.DestinationBinding{"telegram": revoked},
	})
	if restored.Bindings["telegram"].State != StateRestorePending || restored.Bindings["telegram"].Telegram.Target.ChatIdentity != "-100123" {
		t.Fatalf("reverted=%#v", restored.Bindings["telegram"])
	}

	next := engine.Reconcile(context.Background(), Input{
		Namespace: "payments", Project: project, Connection: connection,
		Current: map[string]binding.DestinationBinding{"telegram": revoked},
	})
	if next.Bindings["telegram"].State != StateRegistrationRequired || next.Bindings["telegram"].ResourceName != "new-target" || next.Bindings["telegram"].Telegram != nil {
		t.Fatalf("next=%#v", next.Bindings["telegram"])
	}
}

func TestTelegramGrantRestoreKeepsPinnedTargetAndRequestsExactRouteRestore(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	project.Destinations = map[string]sentryv1alpha1.DestinationSpec{
		"telegram": {Telegram: &sentryv1alpha1.TelegramDestination{Target: "payments-alerts"}},
	}
	connection.Access[0].Destinations = &sentryv1alpha1.DestinationAccess{Telegram: &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"payments-alerts"}}}
	_, selector, err := SelectorFingerprint(connection, project.Destinations["telegram"])
	if err != nil {
		t.Fatal(err)
	}
	current := binding.DestinationBinding{
		Kind: "telegram", SelectorHash: selector, ResourceName: "payments-alerts", State: StateRevoked,
		Telegram: &binding.TelegramBinding{
			Target:   &binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"},
			RouteKey: strings.Repeat("b", 64), RouteRevoked: true,
		},
	}
	result := (&Engine{}).Reconcile(context.Background(), Input{
		Namespace: "payments", Project: project, Connection: connection,
		Current: map[string]binding.DestinationBinding{"telegram": current},
	})
	got := result.Bindings["telegram"]
	if got.State != StateRestorePending || got.Telegram == nil || got.Telegram.Target == nil || *got.Telegram.Target != *current.Telegram.Target {
		t.Fatalf("restored=%#v", got)
	}
}

func TestDuplicateReadableNamesRemainAmbiguous(t *testing.T) {
	t.Parallel()

	project, connection := destinationInput()
	delete(project.Destinations, "oncall")
	catalog := &fakeCatalog{integrations: map[string][]Integration{"slack": {
		{Identity: "one", Name: "company"}, {Identity: "two", Name: "company"},
	}}}
	result := (&Engine{Catalog: catalog}).Reconcile(context.Background(), Input{Namespace: "payments", Project: project, Connection: connection})
	if !resolver.IsKind(result.Statuses["ops"].Err, resolver.ErrorAmbiguous) {
		t.Fatalf("status=%#v", result.Statuses["ops"])
	}
}

func destinationInput() (sentryv1alpha1.SentryProjectSpec, sentryv1alpha1.SentryConnectionSpec) {
	project := sentryv1alpha1.SentryProjectSpec{
		Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		Destinations: map[string]sentryv1alpha1.DestinationSpec{
			"ops":    {Slack: &sentryv1alpha1.SlackDestination{Channel: "#incidents"}},
			"oncall": {PagerDuty: &sentryv1alpha1.PagerDutyDestination{Service: "checkout"}},
		},
	}
	connection := sentryv1alpha1.SentryConnectionSpec{
		Organization: "company",
		Integrations: &sentryv1alpha1.ConnectionIntegrations{
			Slack:     &sentryv1alpha1.SlackIntegration{DefaultWorkspace: "company"},
			PagerDuty: &sentryv1alpha1.PagerDutyIntegration{DefaultAccount: "primary"},
		},
		Access: []sentryv1alpha1.AccessRule{{
			Owner: "payments", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments"}}, Teams: []string{"payments"}, AllowCreate: true,
			Destinations: &sentryv1alpha1.DestinationAccess{
				Slack:     []sentryv1alpha1.SlackDestinationGrant{{Workspace: "company", Channels: []string{"incidents"}}},
				PagerDuty: []sentryv1alpha1.PagerDutyDestinationGrant{{Account: "primary", Services: []string{"checkout"}}},
			},
		}},
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
