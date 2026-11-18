/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package alertpolicy merges one cluster policy with one project's local alert surface.
package alertpolicy

import (
	"errors"
	"maps"
	"slices"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

var (
	ErrSentryMismatch      = errors.New("alert policy uses a different Sentry connection")
	ErrPolicyLocalConflict = errors.New("alert policy and project use the same local key")
	ErrTargetNotFound      = errors.New("alert references a notification target outside its ownership scope")
)

type Result struct {
	Targets        *sentryv1alpha1.NotificationTargets
	Alerts         *map[string]sentryv1alpha1.AlertSpec
	PolicyUID      string
	PolicyName     string
	Generation     int64
	TargetOwners   map[string]binding.ChildOwner
	AlertOwners    map[string]binding.ChildOwner
	PolicyDeleting bool
	Policies       []binding.AppliedAlertPolicy
}

func Merge(project *sentryv1alpha1.SentryProject, policy *sentryv1alpha1.SentryAlertPolicy) (Result, error) {
	if policy == nil {
		return MergeMany(project, nil)
	}
	return MergeMany(project, []*sentryv1alpha1.SentryAlertPolicy{policy})
}

// MergeMany resolves ordered policies and project-local declarations into one immutable
// effective specification. Order is retained for protected rollout state; duplicate keys
// fail closed instead of silently changing ownership or precedence.
func MergeMany(project *sentryv1alpha1.SentryProject, policies []*sentryv1alpha1.SentryAlertPolicy) (Result, error) {
	projectSpec := project.Spec
	projectOwner := binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: project.Name, UID: string(project.UID)}
	if len(policies) == 0 {
		return Result{Targets: cloneTargets(projectSpec.NotificationTargets), Alerts: cloneAlerts(projectSpec.Alerts), TargetOwners: ownersForTargets(projectSpec.NotificationTargets, projectOwner), AlertOwners: ownersForAlerts(localAlerts(projectSpec), projectOwner)}, nil
	}
	localTargets, err := projectSpec.NotificationTargets.Flatten()
	if err != nil {
		return Result{}, err
	}
	if err := validateAlertScope(localAlerts(projectSpec), localTargets); err != nil {
		return Result{}, err
	}
	mergedTargets := &sentryv1alpha1.NotificationTargets{}
	mergedAlerts := map[string]sentryv1alpha1.AlertSpec{}
	targetOwners := map[string]binding.ChildOwner{}
	alertOwners := map[string]binding.ChildOwner{}
	applied := make([]binding.AppliedAlertPolicy, 0, len(policies))
	seenTargets := map[string]bool{}
	seenAlerts := map[string]bool{}
	for _, policy := range policies {
		if policy == nil || projectSpec.Sentry != policy.Spec.Sentry {
			return Result{}, ErrSentryMismatch
		}
		policyTargets, err := policy.Spec.NotificationTargets.Flatten()
		if err != nil {
			return Result{}, err
		}
		if err := validateAlertScope(policy.Spec.Alerts, policyTargets); err != nil {
			return Result{}, err
		}
		for key := range policyTargets {
			_, localConflict := localTargets[key]
			if seenTargets[key] || localConflict {
				return Result{}, ErrPolicyLocalConflict
			}
			seenTargets[key] = true
		}
		for key := range policy.Spec.Alerts {
			if seenAlerts[key] {
				return Result{}, ErrPolicyLocalConflict
			}
			if _, conflict := localAlerts(projectSpec)[key]; conflict {
				return Result{}, ErrPolicyLocalConflict
			}
			seenAlerts[key] = true
		}
		mergedTargets = mergeTargets(mergedTargets, policy.Spec.NotificationTargets)
		maps.Copy(mergedAlerts, policy.Spec.Alerts)
		owner := binding.ChildOwner{Kind: binding.ChildOwnerPolicy, Name: policy.Name, UID: string(policy.UID)}
		maps.Copy(targetOwners, ownersForTargets(policy.Spec.NotificationTargets, owner))
		maps.Copy(alertOwners, ownersForAlerts(policy.Spec.Alerts, owner))
		applied = append(applied, binding.AppliedAlertPolicy{Name: policy.Name, UID: string(policy.UID), Generation: policy.Generation})
	}
	mergedTargets = mergeTargets(mergedTargets, projectSpec.NotificationTargets)
	maps.Copy(mergedAlerts, localAlerts(projectSpec))
	maps.Copy(targetOwners, ownersForTargets(projectSpec.NotificationTargets, projectOwner))
	maps.Copy(alertOwners, ownersForAlerts(localAlerts(projectSpec), projectOwner))
	result := Result{
		Targets: mergedTargets, Alerts: &mergedAlerts, Policies: applied,
		TargetOwners: targetOwners, AlertOwners: alertOwners,
	}
	if len(applied) == 1 {
		result.PolicyUID = applied[0].UID
		result.PolicyName = applied[0].Name
		result.Generation = applied[0].Generation
	}
	return result, nil
}

func ownersForTargets(targets *sentryv1alpha1.NotificationTargets, owner binding.ChildOwner) map[string]binding.ChildOwner {
	result := map[string]binding.ChildOwner{}
	flattened, _ := targets.Flatten()
	for key := range flattened {
		result[key] = owner
	}
	return result
}

func ownersForAlerts(alerts map[string]sentryv1alpha1.AlertSpec, owner binding.ChildOwner) map[string]binding.ChildOwner {
	result := map[string]binding.ChildOwner{}
	for key := range alerts {
		result[key] = owner
	}
	return result
}

func localAlerts(project sentryv1alpha1.SentryProjectSpec) map[string]sentryv1alpha1.AlertSpec {
	if project.Alerts == nil {
		return nil
	}
	return *project.Alerts
}

func validateAlertScope(alerts map[string]sentryv1alpha1.AlertSpec, targets map[string]sentryv1alpha1.DestinationSpec) error {
	for _, alert := range alerts {
		for _, target := range alert.Notify {
			if _, found := targets[target]; !found {
				return ErrTargetNotFound
			}
		}
	}
	return nil
}

func mergeTargets(first, second *sentryv1alpha1.NotificationTargets) *sentryv1alpha1.NotificationTargets {
	result := cloneTargets(first)
	if result == nil {
		result = &sentryv1alpha1.NotificationTargets{}
	}
	copyProvider := func(target map[string][]string, source map[string][]string) map[string][]string {
		if target == nil {
			target = map[string][]string{}
		}
		for integration, names := range source {
			target[integration] = append(target[integration], names...)
		}
		return target
	}
	if second != nil {
		result.Slack = copyProvider(result.Slack, second.Slack)
		result.PagerDuty = copyProvider(result.PagerDuty, second.PagerDuty)
	}
	return result
}

func cloneTargets(input *sentryv1alpha1.NotificationTargets) *sentryv1alpha1.NotificationTargets {
	if input == nil {
		return nil
	}
	result := &sentryv1alpha1.NotificationTargets{Slack: map[string][]string{}, PagerDuty: map[string][]string{}}
	for key, value := range input.Slack {
		result.Slack[key] = slices.Clone(value)
	}
	for key, value := range input.PagerDuty {
		result.PagerDuty[key] = slices.Clone(value)
	}
	return result
}

func cloneAlerts(input *map[string]sentryv1alpha1.AlertSpec) *map[string]sentryv1alpha1.AlertSpec {
	if input == nil {
		return nil
	}
	result := maps.Clone(*input)
	return &result
}
