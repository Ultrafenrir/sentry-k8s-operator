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
	"strings"

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
		if err := authorizeDestinations(project.Destinations, rule.Destinations, connection.Integrations); err != nil {
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
	project.Destinations = nil
	return Evaluate(namespace, project, connection)
}

// EvaluateDestination evaluates project access plus exactly one destination selector.
// Callers use this both before lookup and immediately before protected-state CAS.
func EvaluateDestination(namespace string, project sentryv1alpha1.SentryProjectSpec, connection sentryv1alpha1.SentryConnectionSpec, destination sentryv1alpha1.DestinationSpec) (Decision, error) {
	project.Destinations = map[string]sentryv1alpha1.DestinationSpec{"destination": destination}
	return Evaluate(namespace, project, connection)
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

func authorizeDestinations(destinations map[string]sentryv1alpha1.DestinationSpec, grants *sentryv1alpha1.DestinationAccess, integrations *sentryv1alpha1.ConnectionIntegrations) error {
	if len(destinations) == 0 {
		return nil
	}
	if grants == nil {
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
			if workspace == "" && integrations != nil && integrations.Slack != nil {
				workspace = integrations.Slack.DefaultWorkspace
			}
			channel := strings.TrimPrefix(destination.Slack.Channel, "#")
			allowed := false
			for _, grant := range grants.Slack {
				if grant.Workspace == workspace && containsNormalized(grant.Channels, channel, func(value string) string { return strings.TrimPrefix(value, "#") }) {
					allowed = true
					break
				}
			}
			if !allowed {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		case destination.PagerDuty != nil:
			account := destination.PagerDuty.Account
			if account == "" && integrations != nil && integrations.PagerDuty != nil {
				account = integrations.PagerDuty.DefaultAccount
			}
			allowed := false
			for _, grant := range grants.PagerDuty {
				if grant.Account == account && contains(grant.Services, destination.PagerDuty.Service) {
					allowed = true
					break
				}
			}
			if !allowed {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		case destination.Telegram != nil:
			if grants.Telegram == nil || !contains(grants.Telegram.Targets, destination.Telegram.Target) {
				return &Error{Kind: ErrorDestinationAccessDenied}
			}
		default:
			return &Error{Kind: ErrorDestinationAccessDenied}
		}
	}
	return nil
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
