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

package project

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const sentryProjectNameMaxRunes = 50

var (
	markerComponentPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9.-]*[a-z0-9])?$`)
	managedNamePattern     = regexp.MustCompile(`^(.+) \[managed;o=([a-z](?:[a-z0-9.-]*[a-z0-9])?);m=([a-z](?:[a-z0-9.-]*[a-z0-9])?)\]$`)
	ErrInvalidMarker       = errors.New("managed marker is invalid")
	ErrMarkerNameTooLong   = errors.New("managed display name exceeds Sentry limit")
)

// FormatManagedName encodes the source-confirmed reserved display-name marker.
func FormatManagedName(displayName string, marker binding.Marker) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if marker.Origin != "managed" || displayName == "" || !markerComponentPattern.MatchString(marker.Owner) || !markerComponentPattern.MatchString(marker.ManagedBy) {
		return "", ErrInvalidMarker
	}
	if _, found := ParseManagedName(displayName); found {
		return "", ErrInvalidMarker
	}
	name := fmt.Sprintf("%s [managed;o=%s;m=%s]", displayName, marker.Owner, marker.ManagedBy)
	if utf8.RuneCountInString(name) > sentryProjectNameMaxRunes {
		return "", ErrMarkerNameTooLong
	}
	return name, nil
}

// ParseManagedName accepts only the complete reserved suffix and exact component grammar.
func ParseManagedName(name string) (binding.Marker, bool) {
	matches := managedNamePattern.FindStringSubmatch(name)
	if len(matches) != 4 || strings.TrimSpace(matches[1]) == "" {
		return binding.Marker{}, false
	}
	return binding.Marker{Origin: "managed", Owner: matches[2], ManagedBy: matches[3]}, true
}
