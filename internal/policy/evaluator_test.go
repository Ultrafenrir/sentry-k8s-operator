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

package policy

import (
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestEvaluateDerivesManagedProjectFromNamespace(t *testing.T) {
	t.Parallel()
	project := sentryv1alpha1.SentryProjectSpec{Team: "payments"}
	decision, err := EvaluateProject("checkout-prod", project, sentryv1alpha1.SentryConnectionSpec{})
	if err != nil || decision.AccessMode != "Manage" || decision.Owner != "payments" || !decision.ConsumeDSN {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestEvaluateRejectsMissingTeam(t *testing.T) {
	t.Parallel()
	project := sentryv1alpha1.SentryProjectSpec{}
	if _, err := EvaluateProject("checkout-prod", project, sentryv1alpha1.SentryConnectionSpec{}); !IsKind(err, ErrorProjectInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestEvaluateNotificationTargetsUseConnectionCatalog(t *testing.T) {
	t.Parallel()
	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{
		Team: "payments",
		NotificationTargets: &sentryv1alpha1.NotificationTargets{
			Slack: map[string][]string{"company": {"any-visible-channel"}}, PagerDuty: map[string][]string{"primary": {"Any Visible Service"}},
		},
	}
	if _, err := Evaluate("checkout-prod", project, connection); err != nil {
		t.Fatalf("catalog target: %v", err)
	}
	project.NotificationTargets.Slack = map[string][]string{"undeclared": {"any"}}
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorDestinationUnavailable) {
		t.Fatalf("undeclared workspace error=%v", err)
	}
}

func TestEvaluateProjectIsIndependentFromInvalidDestination(t *testing.T) {
	t.Parallel()
	project := sentryv1alpha1.SentryProjectSpec{
		Team:                "payments",
		NotificationTargets: &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{"missing": {"alerts"}}},
	}
	decision, err := EvaluateProject("checkout-prod", project, testConnection())
	if err != nil || decision.AccessMode != "Manage" {
		t.Fatalf("base decision=%#v err=%v", decision, err)
	}
	if _, err := EvaluateDestination("checkout-prod", project, testConnection(), sentryv1alpha1.DestinationSpec{
		Slack: &sentryv1alpha1.SlackDestination{Workspace: "missing", Channel: "alerts"},
	}); !IsKind(err, ErrorDestinationUnavailable) {
		t.Fatalf("destination error=%v", err)
	}
}

func TestProjectAccessTeamsComeFromProject(t *testing.T) {
	t.Parallel()
	project := sentryv1alpha1.SentryProjectSpec{
		Team:        "payments",
		GrantAccess: &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"backend", "platform"}},
	}
	allowed, err := ProjectAccessTeams(project)
	if err != nil || len(allowed) != 2 || allowed[0] != "backend" || allowed[1] != "platform" {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	project.GrantAccess.Teams = []string{"payments"}
	if _, err := ProjectAccessTeams(project); !IsKind(err, ErrorProjectInvalid) {
		t.Fatalf("primary team repetition error=%v", err)
	}
}

func TestIssueOwnershipTeamsMustBeAttachedByProject(t *testing.T) {
	t.Parallel()
	project := sentryv1alpha1.SentryProjectSpec{
		Team:        "payments",
		GrantAccess: &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}},
		IssueOwnership: &sentryv1alpha1.IssueOwnershipSpec{Rules: []sentryv1alpha1.IssueOwnershipRule{{
			Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"}, Teams: []string{"payments", "backend"},
		}}},
	}
	allowed, err := IssueOwnershipTeams(project)
	if err != nil || len(allowed) != 2 {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
	project.IssueOwnership.Rules[0].Teams = append(project.IssueOwnership.Rules[0].Teams, "security")
	allowed, err = IssueOwnershipTeams(project)
	if !IsKind(err, ErrorProjectInvalid) || len(allowed) != 2 {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
}

func testConnection() sentryv1alpha1.SentryConnectionSpec {
	return sentryv1alpha1.SentryConnectionSpec{Integrations: &sentryv1alpha1.ConnectionIntegrations{
		Slack:     &sentryv1alpha1.SlackIntegration{Workspaces: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "Company Slack"}}},
		PagerDuty: &sentryv1alpha1.PagerDutyIntegration{Accounts: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "primary", SentryName: "Company PagerDuty"}}},
	}}
}
