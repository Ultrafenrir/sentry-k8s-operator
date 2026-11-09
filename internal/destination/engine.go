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

// Package destination resolves human-readable notification destinations and pins their
// private external identities in protected binding state.
package destination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/policy"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/resolver"
)

const (
	StateActive         = "Active"
	StateRevoked        = "Revoked"
	StateCleanupPending = "CleanupPending"
	StateUnresolved     = "Unresolved"
)

var (
	ErrExternalObjectReplaced = errors.New("pinned destination object is missing or replaced")
	ErrExternalNameChanged    = errors.New("pinned destination readable name changed")
	ErrInvalidDestination     = errors.New("destination shape is invalid")
)

type Integration struct {
	Identity string
	Name     string
}

type Resource struct {
	Identity string
	Name     string
}

// Catalog implementations must collect every page before returning candidates.
type Catalog interface {
	ListIntegrations(context.Context, string, string) ([]Integration, error)
	ListSlackChannels(context.Context, string, string) ([]Resource, error)
	ListPagerDutyServices(context.Context, string, string) ([]Resource, error)
}

type Input struct {
	Namespace        string
	Project          sentryv1alpha1.SentryProjectSpec
	Connection       sentryv1alpha1.SentryConnectionSpec
	Current          map[string]binding.DestinationBinding
	Owners           map[string]binding.ChildOwner
	AllowedOwnerUIDs map[string]bool
}

type Status struct {
	Kind   string
	State  string
	Reason string
	Err    error
}

type Result struct {
	Bindings   map[string]binding.DestinationBinding
	Statuses   map[string]Status
	RevokeOnly bool
	PolicyErr  error
}

func (r Result) Err() error {
	errs := make([]error, 0, len(r.Statuses)+1)
	if r.PolicyErr != nil {
		errs = append(errs, r.PolicyErr)
	}
	for _, status := range r.Statuses {
		if status.Err != nil {
			errs = append(errs, status.Err)
		}
	}
	return errors.Join(errs...)
}

type Engine struct {
	Catalog Catalog
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{Bindings: make(map[string]binding.DestinationBinding), Statuses: make(map[string]Status)}
	if _, err := policy.EvaluateProject(input.Namespace, input.Project, input.Connection); err != nil {
		result.RevokeOnly = true
		result.PolicyErr = err
		for name, current := range input.Current {
			current.State = StateRevoked
			current.Reason = "DestinationAccessRevoked"
			result.Bindings[name] = current
			result.Statuses[name] = Status{Kind: current.Kind, State: StateRevoked, Reason: "DestinationAccessRevoked"}
		}
		return result
	}

	desiredTargets, flattenErr := FlattenTargets(input.Project.NotificationTargets)
	if flattenErr != nil {
		result.PolicyErr = flattenErr
		for name, current := range input.Current {
			result.Bindings[name] = current
			result.Statuses[name] = Status{Kind: current.Kind, State: current.State, Reason: "AmbiguousNotificationTarget", Err: flattenErr}
		}
		return result
	}
	names := make([]string, 0, len(desiredTargets))
	for name := range desiredTargets {
		names = append(names, name)
	}
	sort.Strings(names)
	authorized := 0
	denied := 0
	var firstPolicyErr error
	for _, name := range names {
		desired := desiredTargets[name]
		current, found := input.Current[name]
		if _, err := policy.EvaluateDestination(input.Namespace, input.Project, input.Connection, desired); err != nil {
			denied++
			if firstPolicyErr == nil {
				firstPolicyErr = err
			}
			kind, _, _ := SelectorFingerprint(input.Connection, desired)
			status := Status{Kind: kind, State: StateRevoked, Reason: "DestinationAccessRevoked", Err: err}
			if found {
				current.State = StateRevoked
				current.Reason = "DestinationAccessRevoked"
				result.Bindings[name] = current
				status.Kind = current.Kind
			}
			result.Statuses[name] = status
			continue
		}
		authorized++
		if e.Catalog == nil && (desired.Slack != nil || desired.PagerDuty != nil) {
			result.Statuses[name] = Status{Kind: destinationKind(desired), State: StateUnresolved, Reason: "DestinationUnsupported", Err: ErrInvalidDestination}
			if found {
				result.Bindings[name] = current
			}
			continue
		}
		owner := input.Owners[name]
		if (input.Owners != nil && !owner.Valid()) || (found && input.Owners != nil && current.Owner != owner) {
			result.Statuses[name] = Status{Kind: destinationKind(desired), State: StateUnresolved, Reason: "NotificationTargetOwnershipConflict", Err: ErrInvalidDestination}
			if found {
				result.Bindings[name] = current
			}
			continue
		}
		next, status, err := e.resolveOne(ctx, input.Connection, desired, current, found, owner)
		if err != nil {
			status.Err = err
			status.State = StateUnresolved
			result.Statuses[name] = status
			if found {
				result.Bindings[name] = current
				status.State = current.State
				result.Statuses[name] = status
			}
			continue
		}
		result.Bindings[name] = next
		result.Statuses[name] = status
	}
	if denied > 0 && authorized == 0 {
		result.RevokeOnly = true
		result.PolicyErr = firstPolicyErr
	}
	for name, current := range input.Current {
		if _, desired := desiredTargets[name]; desired {
			continue
		}
		if input.AllowedOwnerUIDs != nil && !input.AllowedOwnerUIDs[current.Owner.UID] {
			result.Bindings[name] = current
			result.Statuses[name] = Status{Kind: current.Kind, State: current.State, Reason: "NotificationTargetOwnershipConflict", Err: ErrInvalidDestination}
			continue
		}
		// Preserve the pinned identity until every alert which references the
		// destination has been successfully updated or deleted in Sentry.  This
		// applies to both provider integrations: dropping a binding early would make a failed
		// alert cleanup impossible to authorize and retry safely.
		current.State = StateCleanupPending
		current.Reason = "DestinationRemoved"
		result.Bindings[name] = current
		result.Statuses[name] = Status{Kind: current.Kind, State: StateCleanupPending, Reason: "DestinationRemoved"}
	}
	return result
}

func destinationKind(desired sentryv1alpha1.DestinationSpec) string {
	switch {
	case desired.Slack != nil:
		return "slack"
	case desired.PagerDuty != nil:
		return "pagerduty"
	default:
		return ""
	}
}

func (e *Engine) resolveOne(ctx context.Context, connection sentryv1alpha1.SentryConnectionSpec, desired sentryv1alpha1.DestinationSpec, current binding.DestinationBinding, found bool, owner binding.ChildOwner) (binding.DestinationBinding, Status, error) {
	kind, integrationName, resourceName, err := selector(connection, desired)
	status := Status{Kind: kind}
	if err != nil {
		return binding.DestinationBinding{}, status, err
	}
	hash := selectorHash(kind, integrationName, resourceName)
	provider := kind
	integrations, err := e.Catalog.ListIntegrations(ctx, connection.Organization, provider)
	if err != nil {
		return binding.DestinationBinding{}, status, err
	}
	var integration Integration
	if found && current.Kind == kind && current.SelectorHash == hash {
		integration, err = pinnedIntegration(integrations, current, integrationName)
	} else {
		integration, err = resolver.Exact(integrations, integrationName, kind+"-integration", func(item Integration) string { return item.Name })
	}
	if err != nil {
		return binding.DestinationBinding{}, status, err
	}
	var resources []Resource
	if kind == "slack" {
		resources, err = e.Catalog.ListSlackChannels(ctx, connection.Organization, integration.Identity)
	} else {
		resources, err = e.Catalog.ListPagerDutyServices(ctx, connection.Organization, integration.Identity)
	}
	if err != nil {
		return binding.DestinationBinding{}, status, err
	}
	var resource Resource
	if found && current.Kind == kind && current.SelectorHash == hash {
		resource, err = pinnedResource(resources, current, resourceName, kind == "slack")
	} else if kind == "slack" {
		resource, err = resolver.ExactNormalized(resources, resourceName, "slack-channel", func(item Resource) string { return item.Name }, resolver.SlackChannelName)
	} else {
		resource, err = resolver.Exact(resources, resourceName, "pagerduty-service", func(item Resource) string { return item.Name })
	}
	if err != nil {
		return binding.DestinationBinding{}, status, err
	}
	next := binding.DestinationBinding{
		Owner: owner,
		Kind:  kind, SelectorHash: hash,
		IntegrationIdentity: integration.Identity, IntegrationName: integration.Name,
		ResourceIdentity: resource.Identity, ResourceName: resource.Name,
		State: StateActive,
	}
	return next, Status{Kind: kind, State: StateActive, Reason: "DestinationResolved"}, nil
}

func selector(connection sentryv1alpha1.SentryConnectionSpec, desired sentryv1alpha1.DestinationSpec) (string, string, string, error) {
	switch {
	case desired.Slack != nil && desired.PagerDuty == nil:
		workspace := desired.Slack.Workspace
		if workspace == "" || desired.Slack.Channel == "" {
			return "slack", "", "", ErrInvalidDestination
		}
		return "slack", workspace, resolver.SlackChannelName(desired.Slack.Channel), nil
	case desired.Slack == nil && desired.PagerDuty != nil:
		account := desired.PagerDuty.Account
		if account == "" || desired.PagerDuty.Service == "" {
			return "pagerduty", "", "", ErrInvalidDestination
		}
		return "pagerduty", account, desired.PagerDuty.Service, nil
	default:
		return "", "", "", ErrInvalidDestination
	}
}

func pinnedIntegration(items []Integration, current binding.DestinationBinding, expectedName string) (Integration, error) {
	for _, item := range items {
		if item.Identity != current.IntegrationIdentity {
			continue
		}
		if item.Name != expectedName {
			return Integration{}, ErrExternalNameChanged
		}
		return item, nil
	}
	return Integration{}, ErrExternalObjectReplaced
}

func pinnedResource(items []Resource, current binding.DestinationBinding, expectedName string, slack bool) (Resource, error) {
	for _, item := range items {
		if item.Identity != current.ResourceIdentity {
			continue
		}
		left, right := item.Name, expectedName
		if slack {
			left, right = resolver.SlackChannelName(left), resolver.SlackChannelName(right)
		}
		if left != right {
			return Resource{}, ErrExternalNameChanged
		}
		return item, nil
	}
	return Resource{}, ErrExternalObjectReplaced
}

func selectorHash(kind, integration, resource string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + integration + "\x00" + resource))
	return hex.EncodeToString(digest[:])
}

// SelectorFingerprint returns the canonical kind and protected selector hash for one destination.
func SelectorFingerprint(connection sentryv1alpha1.SentryConnectionSpec, desired sentryv1alpha1.DestinationSpec) (string, string, error) {
	kind, integration, resource, err := selector(connection, desired)
	if err != nil {
		return kind, "", err
	}
	return kind, selectorHash(kind, integration, resource), nil
}
