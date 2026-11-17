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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
)

const sentryProjectNameMaxRunes = 50

var (
	markerComponentPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9.-]*[a-z0-9])?$`)
	managedNamePattern     = regexp.MustCompile(`^(.+) \[managed;o=([a-z](?:[a-z0-9.-]*[a-z0-9])?);m=([a-z](?:[a-z0-9.-]*[a-z0-9])?)\]$`)
	regionalNamePattern    = regexp.MustCompile(`^(.+) \[sko;v=([0-9a-z]+);h=([0-9a-f]{24})\]$`)
	evidenceHashPattern    = regexp.MustCompile(`^[0-9a-f]{24}$`)
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

// FormatRegionalManagedNameFromSlug stores compact regional evidence while keeping the
// external project slug unchanged. Base36 bounds the largest int64 revision to 13 runes.
func FormatRegionalManagedNameFromSlug(slug string, evidence ownership.Evidence) (string, error) {
	return FormatRegionalManagedName(slug, evidence)
}

// FormatRegionalManagedName preserves a requested human-facing name while reserving room
// for complete regional ownership evidence.
func FormatRegionalManagedName(displayName string, evidence ownership.Evidence) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || evidence.Revision <= 0 || !evidenceHashPattern.MatchString(evidence.Fingerprint) {
		return "", ErrInvalidMarker
	}
	suffix := fmt.Sprintf(" [sko;v=%s;h=%s]", strconv.FormatInt(evidence.Revision, 36), evidence.Fingerprint)
	budget := sentryProjectNameMaxRunes - utf8.RuneCountInString(suffix)
	if budget < 1 {
		return "", ErrMarkerNameTooLong
	}
	runes := []rune(displayName)
	if len(runes) > budget {
		runes = runes[:budget]
	}
	return string(runes) + suffix, nil
}

// ParseRegionalManagedName accepts only the complete compact regional suffix.
func ParseRegionalManagedName(name string) (ownership.Evidence, bool) {
	matches := regionalNamePattern.FindStringSubmatch(name)
	if len(matches) != 4 || strings.TrimSpace(matches[1]) == "" {
		return ownership.Evidence{}, false
	}
	revision, err := strconv.ParseInt(matches[2], 36, 64)
	if err != nil || revision <= 0 {
		return ownership.Evidence{}, false
	}
	return ownership.Evidence{Revision: revision, Fingerprint: matches[3]}, true
}

// FormatManagedNameFromSlug derives the Sentry display name from the Kubernetes resource
// name while preserving the complete ownership marker inside Sentry's name limit. The slug
// itself is never truncated.
func FormatManagedNameFromSlug(slug string, marker binding.Marker) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", ErrInvalidMarker
	}
	suffix := fmt.Sprintf(" [managed;o=%s;m=%s]", marker.Owner, marker.ManagedBy)
	budget := sentryProjectNameMaxRunes - utf8.RuneCountInString(suffix)
	if budget < 1 {
		return "", ErrMarkerNameTooLong
	}
	runes := []rune(slug)
	if len(runes) > budget {
		runes = runes[:budget]
	}
	return FormatManagedName(string(runes), marker)
}

// ParseManagedName accepts only the complete reserved suffix and exact component grammar.
func ParseManagedName(name string) (binding.Marker, bool) {
	matches := managedNamePattern.FindStringSubmatch(name)
	if len(matches) != 4 || strings.TrimSpace(matches[1]) == "" {
		return binding.Marker{}, false
	}
	return binding.Marker{Origin: "managed", Owner: matches[2], ManagedBy: matches[3]}, true
}
