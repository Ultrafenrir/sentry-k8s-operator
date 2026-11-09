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

package projectaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
)

func TestEngineGrantsAndRevokesOnlyManagedTeams(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{teams: []Team{{Identity: "team-primary", Slug: "payments"}, {Identity: "team-manual", Slug: "manual"}}}
	sink := &fakeAudit{}
	engine := &Engine{Adapter: adapter, Audit: sink}
	input := testInput()
	input.Desired = []string{"backend", "security"}
	result := engine.Reconcile(context.Background(), input)
	if result.Err != nil {
		t.Fatalf("grant: %v", result.Err)
	}
	if result.Bindings["backend"] == "" || result.Bindings["security"] == "" || len(result.Bindings) != 2 || adapter.grants != 2 {
		t.Fatalf("grant result=%#v adapter=%#v", result, adapter)
	}
	if len(sink.events) != 4 {
		t.Fatalf("grant audit events=%d", len(sink.events))
	}

	input.Current = result.Bindings
	input.Desired = []string{"security"}
	result = engine.Reconcile(context.Background(), input)
	if result.Err != nil {
		t.Fatalf("revoke: %v", result.Err)
	}
	if len(result.Bindings) != 1 || result.Bindings["security"] == "" || adapter.revokes != 1 {
		t.Fatalf("revoke result=%#v adapter=%#v", result, adapter)
	}
	for _, team := range adapter.teams {
		if team.Slug == "backend" {
			t.Fatalf("managed team remained: %#v", adapter.teams)
		}
	}
	if !containsTeam(adapter.teams, "payments") || !containsTeam(adapter.teams, "manual") {
		t.Fatalf("unmanaged teams changed: %#v", adapter.teams)
	}
}

func TestEnginePinsIdentityAndFailsClosedOnReplacement(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{teams: []Team{{Identity: "replacement", Slug: "backend"}}}
	input := testInput()
	input.Desired = []string{"backend"}
	input.Current = map[string]string{"backend": "original"}
	result := (&Engine{Adapter: adapter, Audit: &fakeAudit{}}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err, ErrIdentityChange) || adapter.grants != 0 || adapter.revokes != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineRechecksAuthorizationBeforeMutation(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{teams: []Team{{Identity: "team-primary", Slug: "payments"}}}
	input := testInput()
	input.Desired = []string{"backend"}
	input.Authorize = func(context.Context) error { return errors.New("changed") }
	result := (&Engine{Adapter: adapter, Audit: &fakeAudit{}}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err, ErrAuthorization) || adapter.grants != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineRejectsPrimaryTeamAsAdditionalGrant(t *testing.T) {
	t.Parallel()
	input := testInput()
	input.Desired = []string{"payments"}
	result := (&Engine{Adapter: &fakeAdapter{}, Audit: &fakeAudit{}}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err, ErrInvalidInput) {
		t.Fatalf("error=%v", result.Err)
	}
}

func TestEngineRequiresAuditBeforeMutation(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{teams: []Team{{Identity: "team-primary", Slug: "payments"}}}
	input := testInput()
	input.Desired = []string{"backend"}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err, ErrAuditRequired) || adapter.grants != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func testInput() Input {
	return Input{
		Organization: "company", ProjectSlug: "checkout", PrimaryTeam: "payments",
		Current: map[string]string{}, LocalResource: "checkout-prod/checkout", Generation: 2,
		Authorize: func(context.Context) error { return nil },
	}
}

type fakeAdapter struct {
	teams   []Team
	grants  int
	revokes int
}

func (a *fakeAdapter) ListTeams(context.Context, string, string) ([]Team, error) {
	return append([]Team(nil), a.teams...), nil
}

func (a *fakeAdapter) GrantTeam(_ context.Context, _, _, slug string) error {
	a.grants++
	a.teams = append(a.teams, Team{Identity: "id-" + slug, Slug: slug})
	return nil
}

func (a *fakeAdapter) RevokeTeam(_ context.Context, _, _, identity string) error {
	a.revokes++
	for index, team := range a.teams {
		if team.Identity == identity {
			a.teams = append(a.teams[:index], a.teams[index+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

type fakeAudit struct{ events []audit.Event }

func (a *fakeAudit) Record(_ context.Context, event audit.Event) error {
	if err := audit.Validate(event); err != nil {
		return err
	}
	a.events = append(a.events, event)
	return nil
}

func containsTeam(values []Team, slug string) bool {
	for _, value := range values {
		if value.Slug == slug {
			return true
		}
	}
	return false
}
