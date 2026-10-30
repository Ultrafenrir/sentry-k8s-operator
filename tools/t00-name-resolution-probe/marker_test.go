package nameresolutionprobe

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const sentryProjectNameMaxRunes = 50

var (
	errInvalidMarkerComponent = errors.New("InvalidMarkerComponent")
	errMarkerNameTooLong      = errors.New("MarkerNameTooLong")
	errNestedMarker           = errors.New("NestedMarker")
	markerComponentPattern    = regexp.MustCompile(`^[a-z][a-z0-9.-]*[a-z0-9]$|^[a-z]$`)
	managedMarkerPattern      = regexp.MustCompile(`^(.+) \[managed;o=([a-z][a-z0-9.-]*[a-z0-9]|[a-z]);m=([a-z][a-z0-9.-]*[a-z0-9]|[a-z])\]$`)
)

type managedMarker struct {
	displayName string
	owner       string
	manager     string
}

func formatManagedName(displayName, owner, manager string) (string, error) {
	displayName = strings.TrimSpace(displayName)
	if !markerComponentPattern.MatchString(owner) || !markerComponentPattern.MatchString(manager) {
		return "", errInvalidMarkerComponent
	}
	if displayName == "" {
		return "", errInvalidMarkerComponent
	}
	if _, found := parseManagedName(displayName); found {
		return "", errNestedMarker
	}
	name := fmt.Sprintf("%s [managed;o=%s;m=%s]", displayName, owner, manager)
	if utf8.RuneCountInString(name) > sentryProjectNameMaxRunes {
		return "", errMarkerNameTooLong
	}
	return name, nil
}

func parseManagedName(name string) (managedMarker, bool) {
	matches := managedMarkerPattern.FindStringSubmatch(name)
	if len(matches) != 4 || utf8.RuneCountInString(name) > sentryProjectNameMaxRunes {
		return managedMarker{}, false
	}
	return managedMarker{displayName: matches[1], owner: matches[2], manager: matches[3]}, true
}

func provesOwnership(marker managedMarker, bindingExists, managerClaimMatches bool) bool {
	return marker.owner != "" && marker.manager != "" && bindingExists && managerClaimMatches
}

func TestManagedMarkerRoundTripAndBudget(t *testing.T) {
	name, err := formatManagedName("Checkout", "payments", "company-sentry-op")
	if err != nil {
		t.Fatalf("format marker: %v", err)
	}
	if name != "Checkout [managed;o=payments;m=company-sentry-op]" {
		t.Fatalf("unexpected marker: %q", name)
	}
	if utf8.RuneCountInString(name) != 49 {
		t.Fatalf("expected source-confirmed example length 49, got %d", utf8.RuneCountInString(name))
	}
	marker, found := parseManagedName(name)
	if !found || marker.displayName != "Checkout" || marker.owner != "payments" || marker.manager != "company-sentry-op" {
		t.Fatalf("round trip: marker=%+v found=%v", marker, found)
	}
	if _, err := formatManagedName("CheckoutXX", "payments", "company-sentry-op"); !errors.Is(err, errMarkerNameTooLong) {
		t.Fatalf("over-budget display name: got %v", err)
	}
}

func TestManagedMarkerRejectsInjectionAndNesting(t *testing.T) {
	invalid := []struct {
		owner   string
		manager string
	}{
		{owner: "payments;m=attacker", manager: "company-sentry-op"},
		{owner: "payments", manager: "company]sentry"},
		{owner: "12345", manager: "company-sentry-op"},
		{owner: "Payments", manager: "company-sentry-op"},
	}
	for _, test := range invalid {
		if _, err := formatManagedName("Checkout", test.owner, test.manager); !errors.Is(err, errInvalidMarkerComponent) {
			t.Fatalf("owner=%q manager=%q: got %v", test.owner, test.manager, err)
		}
	}
	marked, err := formatManagedName("Checkout", "payments", "company-sentry-op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := formatManagedName(marked, "payments", "company-sentry-op"); !errors.Is(err, errNestedMarker) {
		t.Fatalf("nested marker: got %v", err)
	}
}

func TestMarkerMustBeExactSuffixAndNeverSoleOwnershipProof(t *testing.T) {
	ordinary := []string{
		"Checkout",
		"Checkout [managed]",
		"[managed;o=payments;m=company-sentry-op] Checkout",
		"Checkout [managed;o=payments;m=company-sentry-op] trailing",
		"Checkout owner=payments managed-by=company-sentry-op",
	}
	for _, name := range ordinary {
		if marker, found := parseManagedName(name); found {
			t.Fatalf("ordinary/lookalike name parsed as marker: %q -> %+v", name, marker)
		}
	}

	name, err := formatManagedName("Checkout", "payments", "company-sentry-op")
	if err != nil {
		t.Fatal(err)
	}
	marker, found := parseManagedName(name)
	if !found {
		t.Fatal("expected exact marker")
	}
	if provesOwnership(marker, false, true) || provesOwnership(marker, true, false) {
		t.Fatal("marker without binding and matching manager claim must not prove ownership")
	}
	if !provesOwnership(marker, true, true) {
		t.Fatal("complete probe evidence should prove ownership")
	}
}
