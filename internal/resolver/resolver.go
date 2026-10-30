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

// Package resolver implements exact, ambiguity-safe selection by readable names.
package resolver

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind is a stable resolver failure classification.
type ErrorKind string

const (
	ErrorNotFound  ErrorKind = "ReferenceNotFound"
	ErrorAmbiguous ErrorKind = "ReferenceAmbiguous"
)

// Error never contains external IDs or candidate data.
type Error struct {
	Kind      ErrorKind
	Reference string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", safeReference(e.Reference), e.Kind)
}

// IsKind reports whether err is a resolver error of kind.
func IsKind(err error, kind ErrorKind) bool {
	var resolveErr *Error
	return errors.As(err, &resolveErr) && resolveErr.Kind == kind
}

// Exact returns the only item whose readable name exactly equals selector.
func Exact[T any](items []T, selector, reference string, name func(T) string) (T, error) {
	return ExactNormalized(items, selector, reference, name, func(value string) string { return value })
}

// Only returns the sole candidate after a caller has already applied policy scope.
func Only[T any](items []T, reference string) (T, error) {
	if len(items) == 1 {
		return items[0], nil
	}
	var zero T
	if len(items) == 0 {
		return zero, &Error{Kind: ErrorNotFound, Reference: reference}
	}
	return zero, &Error{Kind: ErrorAmbiguous, Reference: reference}
}

// ExactNormalized permits only a caller-supplied documented cosmetic normalization.
func ExactNormalized[T any](items []T, selector, reference string, name func(T) string, normalize func(string) string) (T, error) {
	var match T
	matches := 0
	selector = normalize(selector)
	for _, item := range items {
		if normalize(name(item)) == selector {
			match = item
			matches++
		}
	}
	switch matches {
	case 0:
		return match, &Error{Kind: ErrorNotFound, Reference: reference}
	case 1:
		return match, nil
	default:
		var zero T
		return zero, &Error{Kind: ErrorAmbiguous, Reference: reference}
	}
}

// SlackChannelName applies the only alpha cosmetic normalization: one leading '#'.
func SlackChannelName(value string) string {
	return strings.TrimPrefix(value, "#")
}

// ClientKey contains the minimum readable surface required to choose a public DSN key.
// Identity and DSN remain private to the adapter and are deliberately absent here.
type ClientKey struct {
	Name   string
	Active bool
}

// ActiveClientKey selects one active key across all already-collected pages.
func ActiveClientKey(keys []ClientKey, selector string) (ClientKey, error) {
	active := make([]ClientKey, 0, len(keys))
	for _, key := range keys {
		if key.Active {
			active = append(active, key)
		}
	}
	if selector != "" {
		return Exact(active, selector, "client-key", func(key ClientKey) string { return key.Name })
	}
	if len(active) == 1 {
		return active[0], nil
	}
	if len(active) == 0 {
		return ClientKey{}, &Error{Kind: ErrorNotFound, Reference: "client-key"}
	}
	return ClientKey{}, &Error{Kind: ErrorAmbiguous, Reference: "client-key"}
}

func safeReference(reference string) string {
	if reference == "" || len(reference) > 64 {
		return "reference"
	}
	for _, char := range reference {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char)) {
			return "reference"
		}
	}
	return reference
}
