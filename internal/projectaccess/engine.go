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

// Package projectaccess reconciles additional team access without disturbing teams that
// are not owned by the SentryProject grantAccess field.
package projectaccess

import (
	"context"
	"errors"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
)

var (
	ErrAuthorization  = errors.New("project access authorization failed")
	ErrInvalidInput   = errors.New("project access input is invalid")
	ErrIdentityChange = errors.New("project access team identity changed")
	ErrVerification   = errors.New("project access mutation verification failed")
	ErrAuditRequired  = errors.New("project access mutation audit is unavailable")
)

type Team struct {
	Identity string
	Slug     string
}

type Adapter interface {
	ListTeams(context.Context, string, string) ([]Team, error)
	GrantTeam(context.Context, string, string, string) error
	RevokeTeam(context.Context, string, string, string) error
}

type Authorization func(context.Context) error

type Input struct {
	Organization  string
	ProjectSlug   string
	PrimaryTeam   string
	Desired       []string
	Current       map[string]string
	LocalResource string
	Generation    int64
	Authorize     Authorization
}

type Result struct {
	Bindings map[string]string
	Err      error
}

type Engine struct {
	Adapter Adapter
	Audit   audit.Sink
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{Bindings: cloneBindings(input.Current)}
	if e == nil || e.Adapter == nil || input.Organization == "" || input.ProjectSlug == "" || input.PrimaryTeam == "" || input.LocalResource == "" || input.Generation <= 0 || input.Authorize == nil || len(input.Desired) > 50 {
		result.Err = ErrInvalidInput
		return result
	}
	desired, err := desiredSet(input.Desired, input.PrimaryTeam)
	if err != nil {
		result.Err = err
		return result
	}
	observed, err := e.Adapter.ListTeams(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		result.Err = err
		return result
	}
	bySlug, byIdentity, err := indexTeams(observed)
	if err != nil {
		result.Err = err
		return result
	}

	for _, slug := range sortedKeys(desired) {
		pinnedIdentity := result.Bindings[slug]
		if pinnedIdentity != "" {
			if team, found := byIdentity[pinnedIdentity]; found {
				if team.Slug != slug {
					result.Err = ErrIdentityChange
					return result
				}
				continue
			}
			if _, found := bySlug[slug]; found {
				result.Err = ErrIdentityChange
				return result
			}
		}
		if team, found := bySlug[slug]; found {
			result.Bindings[slug] = team.Identity
			continue
		}
		if err := e.mutate(ctx, input, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
			result.Err = err
			return result
		}
		if err := input.Authorize(ctx); err != nil {
			result.Err = errors.Join(ErrAuthorization, err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonAuthorization))
			return result
		}
		if err := e.Adapter.GrantTeam(ctx, input.Organization, input.ProjectSlug, slug); err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonApply))
			return result
		}
		observed, err = e.Adapter.ListTeams(ctx, input.Organization, input.ProjectSlug)
		if err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		bySlug, byIdentity, err = indexTeams(observed)
		if err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		team, found := bySlug[slug]
		if !found {
			result.Err = errors.Join(ErrVerification, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		result.Bindings[slug] = team.Identity
		if err := e.mutate(ctx, input, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
			result.Err = err
			return result
		}
	}

	for _, slug := range sortedKeys(result.Bindings) {
		if desired[slug] {
			continue
		}
		identity := result.Bindings[slug]
		if _, found := byIdentity[identity]; !found {
			delete(result.Bindings, slug)
			continue
		}
		if err := e.mutate(ctx, input, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
			result.Err = err
			return result
		}
		if err := input.Authorize(ctx); err != nil {
			result.Err = errors.Join(ErrAuthorization, err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonAuthorization))
			return result
		}
		if err := e.Adapter.RevokeTeam(ctx, input.Organization, input.ProjectSlug, identity); err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonApply))
			return result
		}
		observed, err = e.Adapter.ListTeams(ctx, input.Organization, input.ProjectSlug)
		if err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		_, byIdentity, err = indexTeams(observed)
		if err != nil {
			result.Err = errors.Join(err, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		if _, found := byIdentity[identity]; found {
			result.Err = errors.Join(ErrVerification, e.mutate(ctx, input, audit.OutcomeFailed, audit.ReasonVerification))
			return result
		}
		delete(result.Bindings, slug)
		if err := e.mutate(ctx, input, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
			result.Err = err
			return result
		}
	}
	return result
}

func (e *Engine) mutate(ctx context.Context, input Input, outcome audit.Outcome, reason audit.Reason) error {
	if e.Audit == nil {
		return ErrAuditRequired
	}
	return e.Audit.Record(ctx, audit.Event{
		Timestamp: time.Now().UTC().Round(0), Component: audit.ComponentProjects, Action: audit.ActionUpdate, Outcome: outcome,
		Reason: reason, LocalResource: input.LocalResource, Revision: input.Generation,
	})
}

func desiredSet(values []string, primary string) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value == primary || len(validation.IsDNS1123Label(value)) != 0 || result[value] {
			return nil, ErrInvalidInput
		}
		result[value] = true
	}
	return result, nil
}

func indexTeams(values []Team) (map[string]Team, map[string]Team, error) {
	bySlug := make(map[string]Team, len(values))
	byIdentity := make(map[string]Team, len(values))
	for _, value := range values {
		if value.Identity == "" || len(validation.IsDNS1123Label(value.Slug)) != 0 {
			return nil, nil, ErrInvalidInput
		}
		if _, found := bySlug[value.Slug]; found {
			return nil, nil, ErrInvalidInput
		}
		if _, found := byIdentity[value.Identity]; found {
			return nil, nil, ErrInvalidInput
		}
		bySlug[value.Slug] = value
		byIdentity[value.Identity] = value
	}
	return bySlug, byIdentity, nil
}

func cloneBindings(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sortedKeys[T any](input map[string]T) []string {
	result := make([]string, 0, len(input))
	for key := range input {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
