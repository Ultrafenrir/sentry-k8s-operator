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

// Package policy derives project capabilities from the SentryProject and validates references
// to connection-owned integration catalogs before any external lookup.
package policy

import (
	"errors"
	"fmt"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

// ErrorKind is a stable project capability or catalog failure classification.
type ErrorKind string

const (
	ErrorProjectInvalid         ErrorKind = "ProjectInvalid"
	ErrorDestinationUnavailable ErrorKind = "DestinationUnavailable"
)

// Error deliberately excludes policy contents and external identities.
type Error struct {
	Kind ErrorKind
}

func (e *Error) Error() string { return fmt.Sprintf("Sentry project capability: %s", e.Kind) }

// IsKind reports whether err has a stable policy classification.
func IsKind(err error, kind ErrorKind) bool {
	var policyErr *Error
	return errors.As(err, &policyErr) && policyErr.Kind == kind
}

// Decision is the non-secret authorization result consumed by reconciliation.
type Decision struct {
	Owner      string
	AccessMode string
	ConsumeDSN bool
}

// Evaluate derives capabilities from the namespaced SentryProject. Kubernetes RBAC is the
// authorization boundary for creating and updating projects; the connection does not maintain
// a second namespace or team allowlist.
func Evaluate(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec) (Decision, error) {
	if namespace == "" || project.Team == "" {
		return Decision{}, &Error{Kind: ErrorProjectInvalid}
	}
	targets, flattenErr := project.NotificationTargets.Flatten()
	if flattenErr != nil {
		return Decision{}, &Error{Kind: ErrorDestinationUnavailable}
	}
	if err := authorizeDestinations(targets, connection.Integrations); err != nil {
		return Decision{}, err
	}
	return Decision{Owner: project.Team, AccessMode: "Manage", ConsumeDSN: true}, nil
}

// EvaluateProject derives the base project mode while deliberately excluding notification
// targets. One invalid target must not stop unrelated project components from reconciling.
func EvaluateProject(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec) (Decision, error) {
	project.NotificationTargets = nil
	project.GrantAccess = nil
	return Evaluate(namespace, project, connection)
}

// ProjectAccessTeams returns the additional teams declared by the project. The
// primary team must not be repeated because it already owns the project in Sentry.
func ProjectAccessTeams(project sentryv1alpha1.SentryProjectSpec) ([]string, error) {
	if project.Team == "" {
		return nil, &Error{Kind: ErrorProjectInvalid}
	}
	if project.GrantAccess == nil {
		return nil, nil
	}
	for _, team := range project.GrantAccess.Teams {
		if team == project.Team {
			return nil, &Error{Kind: ErrorProjectInvalid}
		}
	}
	return append([]string(nil), project.GrantAccess.Teams...), nil
}

// IssueOwnershipTeams returns team references used by issue ownership rules. Every
// referenced team must be the primary project team or be declared through grantAccess.
func IssueOwnershipTeams(project sentryv1alpha1.SentryProjectSpec) ([]string, error) {
	if project.Team == "" || project.IssueOwnership == nil {
		return nil, &Error{Kind: ErrorProjectInvalid}
	}
	attached := map[string]bool{project.Team: true}
	if project.GrantAccess != nil {
		for _, team := range project.GrantAccess.Teams {
			attached[team] = true
		}
	}
	seen := map[string]bool{}
	result := []string{}
	denied := false
	for _, ownershipRule := range project.IssueOwnership.Rules {
		for _, team := range ownershipRule.Teams {
			if !attached[team] {
				denied = true
				continue
			}
			if !seen[team] {
				seen[team] = true
				result = append(result, team)
			}
		}
	}
	if denied {
		return result, &Error{Kind: ErrorProjectInvalid}
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
	if err := authorizeDestinations(map[string]sentryv1alpha1.DestinationSpec{"target": destination}, connection.Integrations); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func authorizeDestinations(destinations map[string]sentryv1alpha1.DestinationSpec, integrations *sentryv1alpha1.ConnectionIntegrations) error {
	if len(destinations) == 0 {
		return nil
	}
	for _, destination := range destinations {
		types := 0
		if destination.Slack != nil {
			types++
		}
		if destination.PagerDuty != nil {
			types++
		}
		if types != 1 {
			return &Error{Kind: ErrorDestinationUnavailable}
		}
		switch {
		case destination.Slack != nil:
			workspace := destination.Slack.Workspace
			if !catalogContains(integrations, "slack", workspace) {
				return &Error{Kind: ErrorDestinationUnavailable}
			}
		case destination.PagerDuty != nil:
			account := destination.PagerDuty.Account
			if !catalogContains(integrations, "pagerduty", account) {
				return &Error{Kind: ErrorDestinationUnavailable}
			}
		default:
			return &Error{Kind: ErrorDestinationUnavailable}
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
