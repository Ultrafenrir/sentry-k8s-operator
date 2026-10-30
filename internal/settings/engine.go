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

// Package settings reconciles only the source-confirmed typed project settings.
package settings

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

var (
	ErrInvalidDesiredState = errors.New("project settings desired state is invalid")
	ErrUnsupportedFilter   = errors.New("required inbound filter is unavailable")
)

type Authorization func(context.Context) error

type Filter struct {
	ID     string `json:"id"`
	Active bool   `json:"active"`
}

type ProjectState struct {
	ScrubData        *bool
	ScrubDefaults    *bool
	ScrubIPAddresses *bool
	SensitiveFields  *[]string
	SafeFields       *[]string
	ETag             string
}

type ProjectPatch struct {
	ScrubData        *bool     `json:"dataScrubber,omitempty"`
	ScrubDefaults    *bool     `json:"dataScrubberDefaults,omitempty"`
	ScrubIPAddresses *bool     `json:"scrubIPAddresses,omitempty"`
	SensitiveFields  *[]string `json:"sensitiveFields,omitempty"`
	SafeFields       *[]string `json:"safeFields,omitempty"`
}

func (p ProjectPatch) Empty() bool {
	return p.ScrubData == nil && p.ScrubDefaults == nil && p.ScrubIPAddresses == nil && p.SensitiveFields == nil && p.SafeFields == nil
}

type Adapter interface {
	ListFilters(context.Context, string, string) ([]Filter, error)
	UpdateFilter(context.Context, string, string, string, bool) error
	GetProjectState(context.Context, string, string) (ProjectState, error)
	UpdateProjectState(context.Context, string, string, ProjectPatch, string) error
}

type Input struct {
	Organization string
	ProjectSlug  string
	Desired      *sentryv1alpha1.ProjectSettings
	Authorize    Authorization
}

type Component struct {
	Managed bool
	Changed int
	Err     error
}

type Result struct {
	Filters Component
	PII     Component
}

func (r Result) Err() error {
	return errors.Join(r.Filters.Err, r.PII.Err)
}

type Engine struct {
	Adapter Adapter
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{}
	if e.Adapter == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" {
		result.Filters.Err = ErrInvalidDesiredState
		return result
	}
	if input.Desired == nil {
		return result
	}
	if err := validate(input.Desired); err != nil {
		if input.Desired.Filters != nil {
			result.Filters = Component{Managed: true, Err: err}
		}
		if input.Desired.PII != nil {
			result.PII = Component{Managed: true, Err: err}
		}
		return result
	}
	if input.Desired.Filters != nil {
		result.Filters.Managed = true
		result.Filters.Changed, result.Filters.Err = e.reconcileFilters(ctx, input, input.Desired.Filters)
	}
	if input.Desired.PII != nil {
		result.PII.Managed = true
		result.PII.Changed, result.PII.Err = e.reconcilePII(ctx, input, input.Desired.PII)
	}
	return result
}

func (e *Engine) reconcileFilters(ctx context.Context, input Input, desired *sentryv1alpha1.FilterSettings) (int, error) {
	filters, err := e.Adapter.ListFilters(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	current := make(map[string]bool, len(filters))
	for _, filter := range filters {
		if filter.ID == "" {
			return 0, ErrUnsupportedFilter
		}
		if _, duplicate := current[filter.ID]; duplicate {
			return 0, ErrUnsupportedFilter
		}
		current[filter.ID] = filter.Active
	}
	wanted := []struct {
		id    string
		value *bool
	}{
		{id: "browser-extensions", value: desired.BrowserExtensions},
		{id: "localhost", value: desired.Localhost},
		{id: "web-crawlers", value: desired.WebCrawlers},
	}
	changed := 0
	for _, item := range wanted {
		if item.value == nil {
			continue
		}
		active, found := current[item.id]
		if !found {
			return changed, fmt.Errorf("%w: %s", ErrUnsupportedFilter, item.id)
		}
		if active == *item.value {
			continue
		}
		if err := input.Authorize(ctx); err != nil {
			return changed, err
		}
		if err := e.Adapter.UpdateFilter(ctx, input.Organization, input.ProjectSlug, item.id, *item.value); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

func (e *Engine) reconcilePII(ctx context.Context, input Input, desired *sentryv1alpha1.PIISettings) (int, error) {
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	patch := ProjectPatch{}
	changed := 0
	if desired.ScrubData != nil && !equalBool(current.ScrubData, desired.ScrubData) {
		patch.ScrubData = desired.ScrubData
		changed++
	}
	if desired.ScrubDefaults != nil && !equalBool(current.ScrubDefaults, desired.ScrubDefaults) {
		patch.ScrubDefaults = desired.ScrubDefaults
		changed++
	}
	if desired.ScrubIPAddresses != nil && !equalBool(current.ScrubIPAddresses, desired.ScrubIPAddresses) {
		patch.ScrubIPAddresses = desired.ScrubIPAddresses
		changed++
	}
	if desired.SensitiveFields != nil && !equalStrings(current.SensitiveFields, desired.SensitiveFields) {
		patch.SensitiveFields = desired.SensitiveFields
		changed++
	}
	if desired.SafeFields != nil && !equalStrings(current.SafeFields, desired.SafeFields) {
		patch.SafeFields = desired.SafeFields
		changed++
	}
	if patch.Empty() {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, patch, current.ETag); err != nil {
		return 0, err
	}
	return changed, nil
}

func validate(desired *sentryv1alpha1.ProjectSettings) error {
	if desired.PII == nil {
		return nil
	}
	for _, list := range []*[]string{desired.PII.SensitiveFields, desired.PII.SafeFields} {
		if list == nil {
			continue
		}
		seen := make(map[string]struct{}, len(*list))
		for _, value := range *list {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
				return ErrInvalidDesiredState
			}
			if _, duplicate := seen[value]; duplicate {
				return ErrInvalidDesiredState
			}
			seen[value] = struct{}{}
		}
	}
	return nil
}

func equalBool(current, desired *bool) bool {
	return current != nil && desired != nil && *current == *desired
}

func equalStrings(current, desired *[]string) bool {
	return current != nil && desired != nil && reflect.DeepEqual(*current, *desired)
}
