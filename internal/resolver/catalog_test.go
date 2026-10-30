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

package resolver

import "testing"

func TestIntegrationResolversAreProviderScopedAndAmbiguitySafe(t *testing.T) {
	t.Parallel()

	items := []Integration{
		{Provider: "slack", Name: "Company"},
		{Provider: "pagerduty", Name: "Company"},
		{Provider: "slack", Name: "Second"},
	}
	item, err := IntegrationByName(items, "pagerduty", "Company", "pagerduty-account")
	if err != nil || item.Provider != "pagerduty" {
		t.Fatalf("provider-scoped result = %#v, %v", item, err)
	}
	if _, err := DefaultIntegration(items, "slack", "slack-workspace"); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("multiple default workspaces = %v, want ambiguous", err)
	}
	item, err = DefaultIntegration(items, "pagerduty", "pagerduty-account")
	if err != nil || item.Name != "Company" {
		t.Fatalf("single default account = %#v, %v", item, err)
	}
}

func TestScopedDestinationResolversRejectDuplicates(t *testing.T) {
	t.Parallel()

	if _, err := SlackChannelByName([]SlackChannel{{Name: "alerts"}, {Name: "#alerts"}}, "alerts"); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("duplicate Slack channel = %v, want ambiguous", err)
	}
	if _, err := PagerDutyServiceByName([]PagerDutyService{{Name: "Primary"}, {Name: "Primary"}}, "Primary"); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("duplicate PagerDuty service = %v, want ambiguous", err)
	}
}

func TestProjectAndTeamUseCanonicalExactSlugs(t *testing.T) {
	t.Parallel()

	if _, err := ProjectBySlug([]Project{{Slug: "checkout-api"}}, "checkout"); !IsKind(err, ErrorNotFound) {
		t.Fatalf("partial project slug = %v", err)
	}
	if _, err := TeamBySlug([]Team{{Slug: "payments", Name: "Payments"}}, "Payments"); !IsKind(err, ErrorNotFound) {
		t.Fatalf("team display name was accepted as slug: %v", err)
	}
}
