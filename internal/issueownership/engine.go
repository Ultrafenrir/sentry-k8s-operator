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

// Package issueownership reconciles the portable, team-only project issue ownership
// contract without exposing Sentry's raw ownership grammar in the public API.
package issueownership

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

var (
	ErrInvalidDesiredState = errors.New("issue ownership desired state is invalid")
	ErrAuthorization       = errors.New("issue ownership authorization failed")
	ErrVerification        = errors.New("issue ownership update verification failed")
	ErrInvalidResponse     = errors.New("issue ownership response is invalid")
)

const maxRawLength = 100_000

const (
	autoAssignIssueOwner     = "Auto Assign to Issue Owner"
	autoAssignSuspectCommits = "Auto Assign to Suspect Commits"
	autoAssignDisabled       = "Turn off Auto-Assignment"
)

type State struct {
	Raw                *string
	Fallthrough        *bool
	AutoAssignment     *string
	CodeownersAutoSync *bool
}

type Patch struct {
	Raw                string `json:"raw"`
	Fallthrough        bool   `json:"fallthrough"`
	AutoAssignment     string `json:"autoAssignment"`
	CodeownersAutoSync bool   `json:"codeownersAutoSync"`
}

type Adapter interface {
	Get(context.Context, string, string) (State, error)
	Update(context.Context, string, string, Patch) error
}

type Authorization func(context.Context) error

type Input struct {
	Organization string
	ProjectSlug  string
	Desired      *sentryv1alpha1.IssueOwnershipSpec
	AllowedTeams []string
	Authorize    Authorization
}

type Result struct {
	Managed bool
	Changed bool
	Err     error
}

type Engine struct {
	Adapter Adapter
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	if input.Desired == nil {
		return Result{}
	}
	result := Result{Managed: true}
	if e == nil || e.Adapter == nil || input.Organization == "" || input.ProjectSlug == "" || input.Authorize == nil {
		result.Err = ErrInvalidDesiredState
		return result
	}
	desired, err := desiredPatch(input.Desired, input.AllowedTeams)
	if err != nil {
		result.Err = err
		return result
	}
	current, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		result.Err = err
		return result
	}
	if err := validateState(current); err != nil {
		result.Err = err
		return result
	}
	if stateMatches(current, desired) {
		return result
	}
	if err := input.Authorize(ctx); err != nil {
		result.Err = errors.Join(ErrAuthorization, err)
		return result
	}
	if err := e.Adapter.Update(ctx, input.Organization, input.ProjectSlug, desired); err != nil {
		result.Err = err
		return result
	}
	observed, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		result.Err = errors.Join(ErrVerification, err)
		return result
	}
	if err := validateState(observed); err != nil || !stateMatches(observed, desired) {
		result.Err = errors.Join(ErrVerification, err)
		return result
	}
	result.Changed = true
	return result
}

func desiredPatch(desired *sentryv1alpha1.IssueOwnershipSpec, allowedTeams []string) (Patch, error) {
	allowed := make(map[string]bool, len(allowedTeams))
	for _, team := range allowedTeams {
		if len(validation.IsDNS1123Label(team)) != 0 || allowed[team] {
			return Patch{}, ErrInvalidDesiredState
		}
		allowed[team] = true
	}
	if len(desired.Rules) > 100 {
		return Patch{}, ErrInvalidDesiredState
	}
	lines := make([]string, 0, len(desired.Rules))
	for _, rule := range desired.Rules {
		matcher, err := renderMatcher(rule.Matcher)
		if err != nil || len(rule.Teams) == 0 || len(rule.Teams) > 10 {
			return Patch{}, ErrInvalidDesiredState
		}
		teams := append([]string(nil), rule.Teams...)
		sort.Strings(teams)
		for index, team := range teams {
			if len(validation.IsDNS1123Label(team)) != 0 || (index > 0 && teams[index-1] == team) {
				return Patch{}, ErrInvalidDesiredState
			}
			if !allowed[team] {
				return Patch{}, ErrAuthorization
			}
			teams[index] = "#" + team
		}
		lines = append(lines, matcher+" "+strings.Join(teams, " "))
	}
	raw := strings.Join(lines, "\n")
	if len(raw) > maxRawLength {
		return Patch{}, ErrInvalidDesiredState
	}
	fallthroughValue := false
	switch desired.Fallthrough {
	case sentryv1alpha1.IssueOwnershipFallthroughAllMembers:
		fallthroughValue = true
	case sentryv1alpha1.IssueOwnershipFallthroughNoOne:
	default:
		return Patch{}, ErrInvalidDesiredState
	}
	autoAssignment := ""
	switch desired.AutoAssignment {
	case sentryv1alpha1.IssueOwnershipAutoAssignIssueOwner:
		autoAssignment = autoAssignIssueOwner
	case sentryv1alpha1.IssueOwnershipAutoAssignSuspectCommits:
		autoAssignment = autoAssignSuspectCommits
	case sentryv1alpha1.IssueOwnershipAutoAssignDisabled:
		autoAssignment = autoAssignDisabled
	default:
		return Patch{}, ErrInvalidDesiredState
	}
	return Patch{Raw: raw, Fallthrough: fallthroughValue, AutoAssignment: autoAssignment, CodeownersAutoSync: desired.CodeownersAutoSync}, nil
}

func renderMatcher(matcher sentryv1alpha1.IssueOwnershipMatcher) (string, error) {
	if !safeToken(matcher.Pattern, 256) {
		return "", ErrInvalidDesiredState
	}
	switch matcher.Type {
	case sentryv1alpha1.IssueOwnershipMatcherPath, sentryv1alpha1.IssueOwnershipMatcherModule, sentryv1alpha1.IssueOwnershipMatcherURL:
		if matcher.TagKey != "" {
			return "", ErrInvalidDesiredState
		}
		return string(matcher.Type) + ":" + matcher.Pattern, nil
	case sentryv1alpha1.IssueOwnershipMatcherTag:
		if !safeTagKey(matcher.TagKey) {
			return "", ErrInvalidDesiredState
		}
		return "tags." + matcher.TagKey + ":" + matcher.Pattern, nil
	default:
		return "", ErrInvalidDesiredState
	}
}

func safeToken(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsAny(value, " \t\r\n\f#\"")
}

func safeTagKey(value string) bool {
	if value == "" || len(value) > 64 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validateState(state State) error {
	if state.Fallthrough == nil || state.AutoAssignment == nil || state.CodeownersAutoSync == nil || state.Raw == nil {
		return ErrInvalidResponse
	}
	switch *state.AutoAssignment {
	case autoAssignIssueOwner, autoAssignSuspectCommits, autoAssignDisabled:
		return nil
	default:
		return fmt.Errorf("%w: auto assignment", ErrInvalidResponse)
	}
}

func stateMatches(state State, desired Patch) bool {
	return normalizeRaw(*state.Raw) == normalizeRaw(desired.Raw) && *state.Fallthrough == desired.Fallthrough &&
		*state.AutoAssignment == desired.AutoAssignment && *state.CodeownersAutoSync == desired.CodeownersAutoSync
}

func normalizeRaw(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}
