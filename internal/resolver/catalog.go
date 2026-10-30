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

// Project is the readable part of a Sentry project candidate.
type Project struct{ Slug string }

// Team is the readable part of a Sentry team candidate. Slug is canonical in alpha.
type Team struct {
	Slug string
	Name string
}

// Integration is a readable, provider-scoped integration candidate.
type Integration struct {
	Provider string
	Name     string
}

// SlackChannel is a readable channel candidate within one already-resolved workspace.
type SlackChannel struct{ Name string }

// PagerDutyService is a readable service candidate within one already-resolved account.
type PagerDutyService struct{ Name string }

func ProjectBySlug(items []Project, slug string) (Project, error) {
	return Exact(items, slug, "project", func(item Project) string { return item.Slug })
}

func TeamBySlug(items []Team, slug string) (Team, error) {
	return Exact(items, slug, "team", func(item Team) string { return item.Slug })
}

// IntegrationByName filters by provider before matching the exact readable name.
func IntegrationByName(items []Integration, provider, name, reference string) (Integration, error) {
	filtered := make([]Integration, 0, len(items))
	for _, item := range items {
		if item.Provider == provider {
			filtered = append(filtered, item)
		}
	}
	return Exact(filtered, name, reference, func(item Integration) string { return item.Name })
}

// DefaultIntegration returns the sole provider candidate when no name is supplied.
func DefaultIntegration(items []Integration, provider, reference string) (Integration, error) {
	filtered := make([]Integration, 0, len(items))
	for _, item := range items {
		if item.Provider == provider {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 1 {
		return filtered[0], nil
	}
	if len(filtered) == 0 {
		return Integration{}, &Error{Kind: ErrorNotFound, Reference: reference}
	}
	return Integration{}, &Error{Kind: ErrorAmbiguous, Reference: reference}
}

func SlackChannelByName(items []SlackChannel, name string) (SlackChannel, error) {
	return ExactNormalized(items, name, "slack-channel", func(item SlackChannel) string { return item.Name }, SlackChannelName)
}

func PagerDutyServiceByName(items []PagerDutyService, name string) (PagerDutyService, error) {
	return Exact(items, name, "pagerduty-service", func(item PagerDutyService) string { return item.Name })
}
