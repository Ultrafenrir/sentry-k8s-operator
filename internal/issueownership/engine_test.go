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

package issueownership

import (
	"context"
	"errors"
	"reflect"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestEngineReconcilesStructuredTeamRulesAndVerifiesReadback(t *testing.T) {
	t.Parallel()

	adapter := &fakeAdapter{state: state("path:old/* #checkout", true, autoAssignDisabled, false)}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout", AllowedTeams: []string{"checkout", "payments"},
		Desired: &sentryv1alpha1.IssueOwnershipSpec{
			Rules: []sentryv1alpha1.IssueOwnershipRule{
				{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/checkout/*"}, Teams: []string{"payments", "checkout"}},
				{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherTag, TagKey: "environment", Pattern: "production"}, Teams: []string{"checkout"}},
			},
			Fallthrough: sentryv1alpha1.IssueOwnershipFallthroughNoOne, AutoAssignment: sentryv1alpha1.IssueOwnershipAutoAssignIssueOwner,
			CodeownersAutoSync: true,
		},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if result.Err != nil || !result.Managed || !result.Changed || authorizations != 1 || adapter.reads != 2 || len(adapter.updates) != 1 {
		t.Fatalf("result=%#v authorizations=%d adapter=%#v", result, authorizations, adapter)
	}
	want := Patch{
		Raw:         "path:src/checkout/* #checkout #payments\ntags.environment:production #checkout",
		Fallthrough: false, AutoAssignment: autoAssignIssueOwner, CodeownersAutoSync: true,
	}
	if !reflect.DeepEqual(adapter.updates[0], want) {
		t.Fatalf("update=%#v want=%#v", adapter.updates[0], want)
	}
}

func TestEngineConvergesWithoutMutationAcrossLineEndings(t *testing.T) {
	t.Parallel()

	adapter := &fakeAdapter{state: state("path:src/* #checkout\r\n", false, autoAssignDisabled, true)}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout", AllowedTeams: []string{"checkout"},
		Desired: &sentryv1alpha1.IssueOwnershipSpec{
			Rules:       []sentryv1alpha1.IssueOwnershipRule{{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"}, Teams: []string{"checkout"}}},
			Fallthrough: sentryv1alpha1.IssueOwnershipFallthroughNoOne, AutoAssignment: sentryv1alpha1.IssueOwnershipAutoAssignDisabled,
			CodeownersAutoSync: true,
		},
		Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Err != nil || result.Changed || adapter.reads != 1 || len(adapter.updates) != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineRejectsUnsafeOrUnauthorizedRulesBeforeRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		desired *sentryv1alpha1.IssueOwnershipSpec
		allowed []string
		want    error
	}{
		{
			name: "unsafe pattern",
			desired: ownershipSpec(sentryv1alpha1.IssueOwnershipRule{
				Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/private files/*"}, Teams: []string{"checkout"},
			}),
			allowed: []string{"checkout"}, want: ErrInvalidDesiredState,
		},
		{
			name: "tag key on path",
			desired: ownershipSpec(sentryv1alpha1.IssueOwnershipRule{
				Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, TagKey: "environment", Pattern: "src/*"}, Teams: []string{"checkout"},
			}),
			allowed: []string{"checkout"}, want: ErrInvalidDesiredState,
		},
		{
			name: "unauthorized team",
			desired: ownershipSpec(sentryv1alpha1.IssueOwnershipRule{
				Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherModule, Pattern: "checkout.*"}, Teams: []string{"security"},
			}),
			allowed: []string{"checkout"}, want: ErrAuthorization,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter := &fakeAdapter{}
			result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
				Organization: "company", ProjectSlug: "checkout", Desired: test.desired, AllowedTeams: test.allowed,
				Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
			})
			if !errors.Is(result.Err, test.want) || adapter.reads != 0 {
				t.Fatalf("result=%#v reads=%d", result, adapter.reads)
			}
		})
	}
}

func TestEngineFailsClosedWhenReadbackDoesNotMatch(t *testing.T) {
	t.Parallel()

	adapter := &fakeAdapter{state: state("", true, autoAssignIssueOwner, true), preserveAfterUpdate: true}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout", AllowedTeams: []string{"checkout"},
		Desired: ownershipSpec(sentryv1alpha1.IssueOwnershipRule{
			Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherURL, Pattern: "https://example.test/*"}, Teams: []string{"checkout"},
		}),
		Authorize: func(context.Context) error { return nil },
	})
	if !errors.Is(result.Err, ErrVerification) || result.Changed {
		t.Fatalf("result=%#v", result)
	}
}

func ownershipSpec(rule sentryv1alpha1.IssueOwnershipRule) *sentryv1alpha1.IssueOwnershipSpec {
	return &sentryv1alpha1.IssueOwnershipSpec{
		Rules: []sentryv1alpha1.IssueOwnershipRule{rule}, Fallthrough: sentryv1alpha1.IssueOwnershipFallthroughNoOne,
		AutoAssignment: sentryv1alpha1.IssueOwnershipAutoAssignDisabled, CodeownersAutoSync: true,
	}
}

func state(raw string, fallthroughValue bool, assignment string, sync bool) State {
	return State{Raw: &raw, Fallthrough: &fallthroughValue, AutoAssignment: &assignment, CodeownersAutoSync: &sync}
}

type fakeAdapter struct {
	state               State
	reads               int
	updates             []Patch
	preserveAfterUpdate bool
}

func (a *fakeAdapter) Get(context.Context, string, string) (State, error) {
	a.reads++
	return a.state, nil
}

func (a *fakeAdapter) Update(_ context.Context, _, _ string, patch Patch) error {
	a.updates = append(a.updates, patch)
	if !a.preserveAfterUpdate {
		a.state = state(patch.Raw, patch.Fallthrough, patch.AutoAssignment, patch.CodeownersAutoSync)
	}
	return nil
}
