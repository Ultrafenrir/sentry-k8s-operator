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

// Package policy evaluates trusted SentryConnection policy before any external lookup.
package policy

import (
	"errors"
	"fmt"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

// ErrorKind is a stable authorization failure classification.
type ErrorKind string

const (
	ErrorAccessDenied            ErrorKind = "AccessDenied"
	ErrorPolicyAmbiguous         ErrorKind = "PolicyAmbiguous"
	ErrorDestinationAccessDenied ErrorKind = "DestinationAccessDenied"
)

// Error deliberately excludes policy contents and external identities.
type Error struct {
	Kind ErrorKind
}

func (e *Error) Error() string { return fmt.Sprintf("Sentry policy: %s", e.Kind) }

// IsKind reports whether err has a stable policy classification.
func IsKind(err error, kind ErrorKind) bool {
	var policyErr *Error
	return errors.As(err, &policyErr) && policyErr.Kind == kind
}

// Decision is the non-secret authorization result consumed by reconciliation.
type Decision struct {
	Owner         string
	AccessMode    string
	ConsumeDSN    bool
	PolicyMatched bool
}

// Evaluate authorizes one SentryProject against the connection's exact namespace rule.
func Evaluate(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec) (Decision, error) {
	rule, err := ruleForNamespace(namespace, connection.Access)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{Owner: rule.Owner, PolicyMatched: true}
	if project.Project.Create != nil {
		if !rule.AllowCreate || !contains(rule.Teams, project.Project.Create.Team) {
			return Decision{}, &Error{Kind: ErrorAccessDenied}
		}
		targets, flattenErr := project.NotificationTargets.Flatten()
		if flattenErr != nil {
			return Decision{}, &Error{Kind: ErrorDestinationAccessDenied}
		}
		if err := authorizeDestinations(targets, rule, connection.Integrations); err != nil {
			return Decision{}, err
		}
		decision.AccessMode = "Manage"
		decision.ConsumeDSN = true
		return decision, nil
	}
	if project.Project.Use == nil || rule.ExistingProjects == nil || !contains(rule.ExistingProjects.Observe, *project.Project.Use) {
		return Decision{}, &Error{Kind: ErrorAccessDenied}
	}
	decision.Owner = ""
	decision.AccessMode = "ReadOnly"
	decision.ConsumeDSN = contains(rule.ExistingProjects.ConsumeDSN, *project.Project.Use)
	if project.DSN != nil && !decision.ConsumeDSN {
		return Decision{}, &Error{Kind: ErrorAccessDenied}
	}
	return decision, nil
}

// EvaluateProject evaluates namespace, owner, project and DSN access while deliberately
// excluding destination grants. Independent destination denial must not stop project,
// settings, DSN, or another allowed destination from reconciling.
func EvaluateProject(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec) (Decision, error) {
	project.NotificationTargets = nil
	project.GrantAccess = nil
	return Evaluate(namespace, project, connection)
}

// AuthorizedProjectAccessTeams returns the independently authorized subset so revoked
// grants can be cleaned up without blocking the project's unrelated components.
func AuthorizedProjectAccessTeams(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec) ([]string, error) {
	decision, err := EvaluateProject(namespace, project, connection)
	if err != nil {
		return nil, err
	}
	if decision.AccessMode != "Manage" || project.Project.Create == nil {
		return nil, &Error{Kind: ErrorAccessDenied}
	}
	rule, err := ruleForNamespace(namespace, connection.Access)
	if err != nil {
		return nil, err
	}
	if project.GrantAccess == nil {
		return nil, nil
	}
	result := make([]string, 0, len(project.GrantAccess.Teams))
	denied := false
	for _, team := range project.GrantAccess.Teams {
		if team == project.Project.Create.Team || !contains(rule.Teams, team) {
			denied = true
			continue
		}
		result = append(result, team)
	}
	if denied {
		return result, &Error{Kind: ErrorAccessDenied}
	}
	return result, nil
}

// EvaluateDestination evaluates project access plus exactly one destination selector.
// Callers use this both before lookup and immediately before protected-state CAS.
func EvaluateDestination(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec, destination sentryv1alpha1.DestinationSpec) (Decision, error) {
	decision, err := EvaluateProject(namespace, project, connection)
	if err != nil {
		return Decision{}, err
	}
	rule, err := ruleForNamespace(namespace, connection.Access)
	if err != nil {
		return Decision{}, err
	}
	if err := authorizeDestinations(map[string]sentryv1alpha1.DestinationSpec{"target": destination}, rule, connection.Integrations); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func ruleForNamespace(namespace string, rules []sentryv1alpha1.AccessRule) (sentryv1alpha1.AccessRule, error) {
	var match sentryv1alpha1.AccessRule
	matches := 0
	for _, rule := range rules {
		if contains(rule.Namespaces.MatchNames, namespace) {
			match = rule
			matches++
		}
	}
	if matches == 0 {
		return match, &Error{Kind: ErrorAccessDenied}
	}
	if matches > 1 {
		return sentryv1alpha1.AccessRule{}, &Error{Kind: ErrorPolicyAmbiguous}
	}
	return match, nil
}

func authorizeDestinations(destinations map[string]sentryv1alpha1.DestinationSpec, rule sentryv1alpha1.AccessRule, integrations *sentryv1alpha1.ConnectionIntegrations) error {
	if len(destinations) == 0 {
		return nil
	}
	if rule.NotificationTargets == nil {
		return &Error{Kind: ErrorDestinationAccessDenied}
	}
	for _, destination := range destinations {
		types := 0
		if destination.Slack != nil {
			types++
		}
		if destination.PagerDuty != nil {
			types++
		}
		if destination.Telegram != nil {
			types++
		}
		if types != 1 {
			return &Error{Kind: ErrorDestinationAccessDenied}
		}
		switch {
		case destination.Slack != nil:
			workspace := destination.Slack.Workspace
			allowed := rule.NotificationTargets != nil && contains(rule.NotificationTargets.SlackWorkspaces, workspace) && catalogContains(integrations, "slack", workspace)
			if !allowed {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		case destination.PagerDuty != nil:
			account := destination.PagerDuty.Account
			allowed := rule.NotificationTargets != nil && contains(rule.NotificationTargets.PagerDutyAccounts, account) && catalogContains(integrations, "pagerduty", account)
			if !allowed {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		case destination.Telegram != nil:
			allowed := rule.NotificationTargets != nil && contains(rule.NotificationTargets.TelegramTargets, destination.Telegram.Target)
			if !allowed {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		default:
			return &Error{Kind: ErrorDestinationAccessDenied}
		}
	}
	return nil
}

func catalogContains(integrations *sentryv1alpha1.ConnectionIntegrations, provider, name string) bool {
	if integrations == nil {
		return false
	}
	var entries []sentryv1alpha1.IntegrationCatalogEntry
	if provider == "slack" && integrations.Slack != nil {
		entries = integrations.Slack.Workspaces
	}
	if provider == "pagerduty" && integrations.PagerDuty != nil {
		entries = integrations.PagerDuty.Accounts
	}
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

func contains(values []string, wanted string) bool {
	return containsNormalized(values, wanted, func(value string) string { return value })
}

func containsNormalized(values []string, wanted string, normalize func(string) string) bool {
	wanted = normalize(wanted)
	for _, value := range values {
		if normalize(value) == wanted {
			return true
		}
	}
	return false
}
