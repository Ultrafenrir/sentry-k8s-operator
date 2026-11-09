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
	"math"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
)

func TestManagedNameExactRoundTripAndBudget(t *testing.T) {
	t.Parallel()

	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	name, err := FormatManagedName("Checkout", marker)
	if err != nil || name != "Checkout [managed;o=payments;m=company-sentry-op]" {
		t.Fatalf("FormatManagedName = %q, %v", name, err)
	}
	parsed, found := ParseManagedName(name)
	if !found || parsed != marker {
		t.Fatalf("ParseManagedName = %#v, %v", parsed, found)
	}
	if _, err := FormatManagedName("CheckoutXX", marker); !errors.Is(err, ErrMarkerNameTooLong) {
		t.Fatalf("over-budget marker error = %v", err)
	}
}

func TestRegionalManagedNameRoundTripKeepsSlugBudget(t *testing.T) {
	t.Parallel()
	evidence, err := ownership.EvidenceFor(ownership.Marker{
		Region: "eu-west-1", Revision: math.MaxInt64, Owner: "payments", Installation: "company-sentry-op",
	})
	if err != nil {
		t.Fatal(err)
	}
	name, err := FormatRegionalManagedNameFromSlug("checkout-service", evidence)
	if err != nil {
		t.Fatalf("format regional name: %v", err)
	}
	if len([]rune(name)) > sentryProjectNameMaxRunes {
		t.Fatalf("regional name exceeds Sentry limit: %q", name)
	}
	parsed, found := ParseRegionalManagedName(name)
	if !found || parsed != evidence {
		t.Fatalf("parsed=%#v found=%v want=%#v", parsed, found, evidence)
	}
	if _, found := ParseRegionalManagedName(name + " trailing"); found {
		t.Fatalf("regional marker accepted trailing data: %q", name)
	}
}

func TestManagedNameFromSlugTruncatesOnlyDisplayPrefix(t *testing.T) {
	t.Parallel()

	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	name, err := FormatManagedNameFromSlug("project-test-resource", marker)
	if err != nil || name != "project-t [managed;o=payments;m=company-sentry-op]" {
		t.Fatalf("FormatManagedNameFromSlug = %q, %v", name, err)
	}
	parsed, found := ParseManagedName(name)
	if !found || parsed != marker {
		t.Fatalf("ParseManagedName = %#v, %v", parsed, found)
	}
}

func TestManagedNameRejectsLookalikesInjectionAndNesting(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"Checkout",
		"[managed;o=payments;m=company-sentry-op] Checkout",
		"Checkout [managed;o=payments;m=company-sentry-op] trailing",
		"Checkout owner=payments managed-by=company-sentry-op",
	} {
		if _, found := ParseManagedName(name); found {
			t.Errorf("lookalike parsed as marker: %q", name)
		}
	}
	marker := binding.Marker{Origin: "managed", Owner: "payments;evil", ManagedBy: "company-sentry-op"}
	if _, err := FormatManagedName("Checkout", marker); !errors.Is(err, ErrInvalidMarker) {
		t.Fatalf("delimiter injection error = %v", err)
	}
	marker.Owner = "payments"
	marked, _ := FormatManagedName("Checkout", marker)
	if _, err := FormatManagedName(marked, marker); !errors.Is(err, ErrInvalidMarker) {
		t.Fatalf("nested marker error = %v", err)
	}
}
