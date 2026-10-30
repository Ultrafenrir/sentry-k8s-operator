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

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestEvaluateManagedCreateAndDestinationGrants(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{
		Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		Destinations: map[string]sentryv1alpha1.DestinationSpec{
			"chat": {Slack: &sentryv1alpha1.SlackDestination{Channel: "#payments-alerts"}},
			"page": {PagerDuty: &sentryv1alpha1.PagerDutyDestination{Service: "Payments Primary"}},
			"bot":  {Telegram: &sentryv1alpha1.TelegramDestination{Target: "payments-oncall"}},
		},
	}
	decision, err := Evaluate("checkout-prod", project, connection)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Owner != "payments" || decision.AccessMode != "Manage" || !decision.ConsumeDSN {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestEvaluateDeniesBeforeDestinationResolution(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{
		Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		Destinations: map[string]sentryv1alpha1.DestinationSpec{
			"chat": {Slack: &sentryv1alpha1.SlackDestination{Workspace: "Other Slack", Channel: "payments-alerts"}},
		},
	}
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorDestinationAccessDenied) {
		t.Fatalf("error = %v, want destination denied", err)
	}
}

func TestEvaluateProjectKeepsBaseAccessIndependentFromDestinationGrant(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{
		Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		Destinations: map[string]sentryv1alpha1.DestinationSpec{
			"denied": {Slack: &sentryv1alpha1.SlackDestination{Workspace: "Other Slack", Channel: "payments-alerts"}},
		},
	}
	decision, err := EvaluateProject("checkout-prod", project, connection)
	if err != nil || decision.AccessMode != "Manage" || decision.Owner != "payments" {
		t.Fatalf("base decision=%#v err=%v", decision, err)
	}
	if _, err := EvaluateDestination("checkout-prod", project, connection, project.Destinations["denied"]); !IsKind(err, ErrorDestinationAccessDenied) {
		t.Fatalf("destination error=%v", err)
	}
}

func TestEvaluateDefensivelyRejectsMultipleDestinationTypes(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{
		Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		Destinations: map[string]sentryv1alpha1.DestinationSpec{
			"invalid": {
				Slack:    &sentryv1alpha1.SlackDestination{Channel: "payments-alerts"},
				Telegram: &sentryv1alpha1.TelegramDestination{Target: "payments-oncall"},
			},
		},
	}
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorDestinationAccessDenied) {
		t.Fatalf("multiple destination types error = %v", err)
	}
}

func TestEvaluateExternalRequiresObserveAndSeparateDSNGrant(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	observed := "shared-platform"
	project := sentryv1alpha1.SentryProjectSpec{Project: sentryv1alpha1.ProjectSelector{Use: &observed}}
	decision, err := Evaluate("checkout-prod", project, connection)
	if err != nil {
		t.Fatalf("observe-only: %v", err)
	}
	if decision.AccessMode != "ReadOnly" || decision.Owner != "" || decision.ConsumeDSN {
		t.Fatalf("observe-only decision = %#v", decision)
	}
	project.DSN = &sentryv1alpha1.DSNSpec{}
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorAccessDenied) {
		t.Fatalf("DSN without grant error = %v", err)
	}

	consumable := "checkout-api"
	project.Project.Use = &consumable
	decision, err = Evaluate("checkout-prod", project, connection)
	if err != nil || !decision.ConsumeDSN {
		t.Fatalf("consume decision = %#v, %v", decision, err)
	}
}

func TestEvaluateRejectsMissingAndAmbiguousNamespacePolicy(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}}}
	if _, err := Evaluate("unknown", project, connection); !IsKind(err, ErrorAccessDenied) {
		t.Fatalf("missing rule error = %v", err)
	}
	duplicate := connection.Access[0]
	duplicate.Owner = "other"
	connection.Access = append(connection.Access, duplicate)
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorPolicyAmbiguous) {
		t.Fatalf("overlapping rule error = %v", err)
	}
}

func TestEvaluateRequiresExactTeam(t *testing.T) {
	t.Parallel()

	connection := testConnection()
	project := sentryv1alpha1.SentryProjectSpec{Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "Payments"}}}
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorAccessDenied) {
		t.Fatalf("case-insensitive team accepted: %v", err)
	}
	project.Project.Create.Team = "pay"
	if _, err := Evaluate("checkout-prod", project, connection); !IsKind(err, ErrorAccessDenied) {
		t.Fatalf("partial team accepted: %v", err)
	}
}

func testConnection() sentryv1alpha1.SentryConnectionSpec {
	return sentryv1alpha1.SentryConnectionSpec{
		Access: []sentryv1alpha1.AccessRule{{
			Owner:       "payments",
			Namespaces:  sentryv1alpha1.NamespaceAccess{MatchNames: []string{"checkout-prod"}},
			Teams:       []string{"payments"},
			AllowCreate: true,
			ExistingProjects: &sentryv1alpha1.ExistingProjectAccess{
				Observe:    []string{"checkout-api", "shared-platform"},
				ConsumeDSN: []string{"checkout-api"},
			},
			Destinations: &sentryv1alpha1.DestinationAccess{
				Slack:     []sentryv1alpha1.SlackDestinationGrant{{Workspace: "Company Slack", Channels: []string{"payments-alerts"}}},
				PagerDuty: []sentryv1alpha1.PagerDutyDestinationGrant{{Account: "Company PagerDuty", Services: []string{"Payments Primary"}}},
				Telegram:  &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"payments-oncall"}},
			},
		}},
		Integrations: &sentryv1alpha1.ConnectionIntegrations{
			Slack:     &sentryv1alpha1.SlackIntegration{DefaultWorkspace: "Company Slack"},
			PagerDuty: &sentryv1alpha1.PagerDutyIntegration{DefaultAccount: "Company PagerDuty"},
		},
	}
}
