package nameresolutionprobe

import (
	"errors"
	"strings"
	"testing"
)

var (
	errReferenceNotFound   = errors.New("ReferenceNotFound")
	errReferenceAmbiguous  = errors.New("ReferenceAmbiguous")
	errDSNKeyAmbiguous     = errors.New("DSNKeyAmbiguous")
	errExternalNameChanged = errors.New("ExternalNameChanged")
	errExternalMissing     = errors.New("ExternalObjectMissing")
	errPermissionDenied    = errors.New("PermissionDenied")
)

type candidate struct {
	identity string
	name     string
	scope    string
	active   bool
}

func resolveExact(candidates []candidate, name, scope string) (candidate, error) {
	var matches []candidate
	for _, item := range candidates {
		if item.scope == scope && item.name == name {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return candidate{}, errReferenceNotFound
	case 1:
		return matches[0], nil
	default:
		return candidate{}, errReferenceAmbiguous
	}
}

func resolveSlackChannel(candidates []candidate, channel, workspace string) (candidate, error) {
	return resolveExact(candidates, strings.TrimPrefix(channel, "#"), workspace)
}

func resolveClientKey(keys []candidate, selector string) (candidate, error) {
	active := make([]candidate, 0, len(keys))
	for _, key := range keys {
		if key.active {
			active = append(active, key)
		}
	}
	if selector == "" {
		switch len(active) {
		case 0:
			return candidate{}, errReferenceNotFound
		case 1:
			return active[0], nil
		default:
			return candidate{}, errDSNKeyAmbiguous
		}
	}

	var matches []candidate
	for _, key := range active {
		if key.name == selector {
			matches = append(matches, key)
		}
	}
	switch len(matches) {
	case 0:
		return candidate{}, errReferenceNotFound
	case 1:
		return matches[0], nil
	default:
		return candidate{}, errDSNKeyAmbiguous
	}
}

func validatePinned(candidates []candidate, identity, expectedName string) error {
	for _, item := range candidates {
		if item.identity != identity {
			continue
		}
		if item.name != expectedName {
			return errExternalNameChanged
		}
		return nil
	}
	return errExternalMissing
}

func classifyHTTPStatus(status int) error {
	if status == 401 || status == 403 {
		return errPermissionDenied
	}
	return nil
}

func TestExactScopedResolution(t *testing.T) {
	candidates := []candidate{
		{identity: "internal-a", name: "payments-alerts", scope: "Company Slack"},
		{identity: "internal-b", name: "payments-alerts", scope: "Other Slack"},
	}
	resolved, err := resolveExact(candidates, "payments-alerts", "Company Slack")
	if err != nil || resolved.identity != "internal-a" {
		t.Fatalf("scoped unique resolution: resolved=%+v err=%v", resolved, err)
	}
	if _, err := resolveExact(candidates, "Payments-Alerts", "Company Slack"); !errors.Is(err, errReferenceNotFound) {
		t.Fatalf("matching must remain case-sensitive and exact, got %v", err)
	}
	if _, err := resolveExact(candidates, "missing", "Company Slack"); !errors.Is(err, errReferenceNotFound) {
		t.Fatalf("missing reference: got %v", err)
	}
}

func TestDuplicateReadableNamesAreAmbiguous(t *testing.T) {
	candidates := []candidate{
		{identity: "internal-a", name: "Company Slack", scope: "organization"},
		{identity: "internal-b", name: "Company Slack", scope: "organization"},
	}
	if _, err := resolveExact(candidates, "Company Slack", "organization"); !errors.Is(err, errReferenceAmbiguous) {
		t.Fatalf("duplicate readable names: got %v", err)
	}
}

func TestSlackChannelCosmeticNormalizationOnly(t *testing.T) {
	candidates := []candidate{
		{identity: "channel-a", name: "payments-alerts", scope: "Company Slack"},
		{identity: "channel-b", name: "payments-alerts", scope: "Other Slack"},
	}
	resolved, err := resolveSlackChannel(candidates, "#payments-alerts", "Company Slack")
	if err != nil || resolved.identity != "channel-a" {
		t.Fatalf("Slack # normalization: resolved=%+v err=%v", resolved, err)
	}
}

func TestClientKeySelectionAcrossCombinedPages(t *testing.T) {
	keys := []candidate{
		{identity: "key-page-1", name: "Default", active: true},
		{identity: "key-page-2", name: "Default", active: true},
		{identity: "inactive", name: "Old", active: false},
	}
	if _, err := resolveClientKey(keys, ""); !errors.Is(err, errDSNKeyAmbiguous) {
		t.Fatalf("multiple active keys without selector: got %v", err)
	}
	if _, err := resolveClientKey(keys, "Default"); !errors.Is(err, errDSNKeyAmbiguous) {
		t.Fatalf("duplicate labels across pages: got %v", err)
	}

	unique := []candidate{
		{identity: "key-a", name: "Primary", active: true},
		{identity: "key-b", name: "Old", active: false},
	}
	resolved, err := resolveClientKey(unique, "")
	if err != nil || resolved.identity != "key-a" {
		t.Fatalf("single active key: resolved=%+v err=%v", resolved, err)
	}
	if _, err := resolveClientKey(unique, "Missing"); !errors.Is(err, errReferenceNotFound) {
		t.Fatalf("missing explicit key label: got %v", err)
	}
}

func TestPinnedIdentityNeverRebindsByName(t *testing.T) {
	replacement := []candidate{{identity: "new-id", name: "payments-alerts", scope: "Company Slack"}}
	if err := validatePinned(replacement, "old-id", "payments-alerts"); !errors.Is(err, errExternalMissing) {
		t.Fatalf("same-name replacement must not rebind: got %v", err)
	}
	renamed := []candidate{{identity: "old-id", name: "payments-renamed", scope: "Company Slack"}}
	if err := validatePinned(renamed, "old-id", "payments-alerts"); !errors.Is(err, errExternalNameChanged) {
		t.Fatalf("external rename: got %v", err)
	}
}

func TestPermissionErrorsRemainDistinct(t *testing.T) {
	for _, status := range []int{401, 403} {
		if err := classifyHTTPStatus(status); !errors.Is(err, errPermissionDenied) {
			t.Fatalf("status %d: got %v", status, err)
		}
	}
	if err := classifyHTTPStatus(404); err != nil {
		t.Fatalf("404 is not authorization failure: got %v", err)
	}
}
