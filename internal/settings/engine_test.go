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

package settings

import (
	"context"
	"errors"
	"reflect"
	"testing"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestEngineReconcilesOnlyExplicitFieldsAndWholeLists(t *testing.T) {
	t.Parallel()

	trueValue := true
	falseValue := false
	empty := []string{}
	adapter := &fakeAdapter{
		filters: []Filter{
			{ID: "browser-extensions", Active: false},
			{ID: "localhost", Active: true},
			{ID: "web-crawlers", Active: false},
		},
		state: ProjectState{
			ScrubData:       &falseValue,
			SensitiveFields: stringSlicePointer([]string{"manual-old"}),
			SafeFields:      stringSlicePointer([]string{"manual-neighbor"}),
			ETag:            `"revision-7"`,
		},
	}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue, Localhost: &falseValue},
			PII:     &sentryv1alpha1.PIISettings{ScrubData: &trueValue, SensitiveFields: &empty},
		},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Filters.Changed != 2 || result.PII.Changed != 2 || authorizations != 3 {
		t.Fatalf("result=%#v authorizations=%d", result, authorizations)
	}
	if !reflect.DeepEqual(adapter.filterUpdates, []filterUpdate{{id: "browser-extensions", active: true}, {id: "localhost", active: false}}) {
		t.Fatalf("filter updates = %#v", adapter.filterUpdates)
	}
	if len(adapter.projectUpdates) != 1 {
		t.Fatalf("project updates = %#v", adapter.projectUpdates)
	}
	patch := adapter.projectUpdates[0]
	if patch.etag != `"revision-7"` || patch.patch.ScrubData == nil || !*patch.patch.ScrubData || patch.patch.SensitiveFields == nil || len(*patch.patch.SensitiveFields) != 0 {
		t.Fatalf("patch = %#v", patch)
	}
	if patch.patch.SafeFields != nil || patch.patch.ScrubDefaults != nil || patch.patch.ScrubIPAddresses != nil {
		t.Fatalf("unmanaged neighboring fields were included: %#v", patch.patch)
	}
}

func TestEngineValidatesBeforeReadsOrWrites(t *testing.T) {
	t.Parallel()

	invalid := []string{"authorization", "authorization"}
	adapter := &fakeAdapter{}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      &sentryv1alpha1.ProjectSettings{PII: &sentryv1alpha1.PIISettings{SensitiveFields: &invalid}},
		Authorize:    func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if !errors.Is(result.Err(), ErrInvalidDesiredState) || adapter.reads != 0 || len(adapter.projectUpdates) != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineRemovedBlocksStopManagementWithoutReset(t *testing.T) {
	t.Parallel()

	adapter := &fakeAdapter{}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      nil,
		Authorize:    func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Filters.Managed || result.PII.Managed || result.Err() != nil || adapter.reads != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineContinuesIndependentPIIAfterFilterComponentFailure(t *testing.T) {
	t.Parallel()

	trueValue := true
	falseValue := false
	adapter := &fakeAdapter{
		filters: []Filter{{ID: "localhost", Active: true}},
		state:   ProjectState{ScrubData: &falseValue},
	}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue},
			PII:     &sentryv1alpha1.PIISettings{ScrubData: &trueValue},
		},
		Authorize: func(context.Context) error { return nil },
	})
	if !errors.Is(result.Filters.Err, ErrUnsupportedFilter) || result.PII.Err != nil || result.PII.Changed != 1 || len(adapter.projectUpdates) != 1 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineAuthorizationChangePreventsMutation(t *testing.T) {
	t.Parallel()

	trueValue := true
	adapter := &fakeAdapter{filters: []Filter{{ID: "browser-extensions", Active: false}}}
	denied := errors.New("policy changed")
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      &sentryv1alpha1.ProjectSettings{Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue}},
		Authorize:    func(context.Context) error { return denied },
	})
	if !errors.Is(result.Filters.Err, denied) || len(adapter.filterUpdates) != 0 {
		t.Fatalf("result=%#v updates=%#v", result, adapter.filterUpdates)
	}
}

type filterUpdate struct {
	id     string
	active bool
}

type projectUpdate struct {
	patch ProjectPatch
	etag  string
}

type fakeAdapter struct {
	filters        []Filter
	state          ProjectState
	reads          int
	filterUpdates  []filterUpdate
	projectUpdates []projectUpdate
}

func (a *fakeAdapter) ListFilters(context.Context, string, string) ([]Filter, error) {
	a.reads++
	return append([]Filter(nil), a.filters...), nil
}

func (a *fakeAdapter) UpdateFilter(_ context.Context, _, _, id string, active bool) error {
	a.filterUpdates = append(a.filterUpdates, filterUpdate{id: id, active: active})
	return nil
}

func (a *fakeAdapter) GetProjectState(context.Context, string, string) (ProjectState, error) {
	a.reads++
	return a.state, nil
}

func (a *fakeAdapter) UpdateProjectState(_ context.Context, _, _ string, patch ProjectPatch, etag string) error {
	a.projectUpdates = append(a.projectUpdates, projectUpdate{patch: patch, etag: etag})
	return nil
}

func stringSlicePointer(value []string) *[]string { return &value }
