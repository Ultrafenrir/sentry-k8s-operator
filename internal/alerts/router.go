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

package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

const LegacyCombinedAdapter = "legacy-alerts-24.7.1"

type legacyRoutedIdentity struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// LegacyRouterAdapter presents the two Sentry 24.7.1 APIs as one pinned capability while
// retaining the native family in the protected external identity.
type LegacyRouterAdapter struct {
	Legacy *LegacyHTTPAdapter
}

func (a *LegacyRouterAdapter) Ensure(ctx context.Context, adapter, organization, projectSlug, key string, desired Desired, marker binding.Marker) (Observed, error) {
	child, kind, err := a.childForDesired(adapter, desired)
	if err != nil {
		return Observed{}, err
	}
	observed, err := a.Legacy.Ensure(ctx, child, organization, projectSlug, key, desired, marker)
	if err != nil {
		return Observed{}, err
	}
	return routeLegacyObserved(kind, observed)
}

func (a *LegacyRouterAdapter) Observe(ctx context.Context, adapter, organization, projectSlug, identity string) (Observed, bool, error) {
	routed, child, err := parseLegacyRoutedIdentity(adapter, identity)
	if err != nil || a.Legacy == nil {
		return Observed{}, false, errors.Join(ErrLegacyInvalidState, err)
	}
	observed, found, err := a.Legacy.Observe(ctx, child, organization, projectSlug, routed.ID)
	if err != nil || !found {
		return Observed{}, found, err
	}
	wrapped, err := routeLegacyObserved(routed.Kind, observed)
	return wrapped, err == nil, err
}

func (a *LegacyRouterAdapter) Update(ctx context.Context, adapter, organization, projectSlug, identity string, desired Desired) (Observed, error) {
	routed, child, err := parseLegacyRoutedIdentity(adapter, identity)
	if err != nil || a.Legacy == nil || child != legacyChildForTrigger(desired.Trigger) {
		return Observed{}, errors.Join(ErrUnsupportedUpdate, err)
	}
	observed, err := a.Legacy.Update(ctx, child, organization, projectSlug, routed.ID, desired)
	if err != nil {
		return Observed{}, err
	}
	return routeLegacyObserved(routed.Kind, observed)
}

func (a *LegacyRouterAdapter) SetEnabled(ctx context.Context, adapter, organization, projectSlug, identity string, enabled bool) (Observed, error) {
	routed, child, err := parseLegacyRoutedIdentity(adapter, identity)
	if err != nil || a.Legacy == nil {
		return Observed{}, errors.Join(ErrLegacyToggleUnsupported, err)
	}
	observed, err := a.Legacy.SetEnabled(ctx, child, organization, projectSlug, routed.ID, enabled)
	if err != nil {
		return Observed{}, err
	}
	return routeLegacyObserved(routed.Kind, observed)
}

func (a *LegacyRouterAdapter) Delete(ctx context.Context, adapter, organization, projectSlug, identity string, marker binding.Marker) error {
	routed, child, err := parseLegacyRoutedIdentity(adapter, identity)
	if err != nil || a.Legacy == nil {
		return errors.Join(ErrLegacyInvalidState, err)
	}
	return a.Legacy.Delete(ctx, child, organization, projectSlug, routed.ID, marker)
}

func (a *LegacyRouterAdapter) childForDesired(adapter string, desired Desired) (string, string, error) {
	if adapter != LegacyCombinedAdapter || a.Legacy == nil {
		return "", "", ErrLegacyInvalidState
	}
	child := legacyChildForTrigger(desired.Trigger)
	switch child {
	case LegacyIssueAdapter:
		return child, "issue", nil
	case LegacyMetricAdapter:
		return child, "metric", nil
	default:
		return "", "", ErrLegacyInvalidState
	}
}

func legacyChildForTrigger(trigger Trigger) string {
	switch trigger {
	case TriggerFirstSeen, TriggerRegression:
		return LegacyIssueAdapter
	case TriggerEventCount, TriggerUserCount:
		return LegacyMetricAdapter
	default:
		return ""
	}
}

func routeLegacyObserved(kind string, observed Observed) (Observed, error) {
	expected := LegacyIssueAdapter
	if kind == "metric" {
		expected = LegacyMetricAdapter
	}
	if (kind != "issue" && kind != "metric") || observed.Adapter != expected {
		return Observed{}, ErrLegacyInvalidState
	}
	identity, err := encodeLegacyRoutedIdentity(legacyRoutedIdentity{Kind: kind, ID: observed.Identity})
	if err != nil {
		return Observed{}, err
	}
	observed.Identity = identity
	observed.Adapter = LegacyCombinedAdapter
	return observed, nil
}

func encodeLegacyRoutedIdentity(identity legacyRoutedIdentity) (string, error) {
	if (identity.Kind != "issue" && identity.Kind != "metric") || !numericIdentityPattern.MatchString(identity.ID) {
		return "", ErrLegacyInvalidState
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", ErrLegacyInvalidState
	}
	return string(payload), nil
}

func parseLegacyRoutedIdentity(adapter, value string) (legacyRoutedIdentity, string, error) {
	if adapter != LegacyCombinedAdapter {
		return legacyRoutedIdentity{}, "", ErrLegacyInvalidState
	}
	var identity legacyRoutedIdentity
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return legacyRoutedIdentity{}, "", ErrLegacyInvalidState
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return legacyRoutedIdentity{}, "", ErrLegacyInvalidState
	}
	child := ""
	switch identity.Kind {
	case "issue":
		child = LegacyIssueAdapter
	case "metric":
		child = LegacyMetricAdapter
	}
	if child == "" || !numericIdentityPattern.MatchString(identity.ID) {
		return legacyRoutedIdentity{}, "", ErrLegacyInvalidState
	}
	return identity, child, nil
}

// MuxAdapter dispatches protected identities by their already-pinned adapter name. It lets
// the controller keep deleting old children safely after a compatibility profile changes.
type MuxAdapter struct {
	Adapters map[string]Adapter
}

func (m *MuxAdapter) adapter(name string) (Adapter, error) {
	adapter := m.Adapters[name]
	if adapter == nil {
		return nil, ErrAdapterUnavailable
	}
	return adapter, nil
}

func (m *MuxAdapter) Ensure(ctx context.Context, adapter, organization, projectSlug, key string, desired Desired, marker binding.Marker) (Observed, error) {
	target, err := m.adapter(adapter)
	if err != nil {
		return Observed{}, err
	}
	return target.Ensure(ctx, adapter, organization, projectSlug, key, desired, marker)
}

func (m *MuxAdapter) Observe(ctx context.Context, adapter, organization, projectSlug, identity string) (Observed, bool, error) {
	target, err := m.adapter(adapter)
	if err != nil {
		return Observed{}, false, err
	}
	return target.Observe(ctx, adapter, organization, projectSlug, identity)
}

func (m *MuxAdapter) Update(ctx context.Context, adapter, organization, projectSlug, identity string, desired Desired) (Observed, error) {
	target, err := m.adapter(adapter)
	if err != nil {
		return Observed{}, err
	}
	return target.Update(ctx, adapter, organization, projectSlug, identity, desired)
}

func (m *MuxAdapter) SetEnabled(ctx context.Context, adapter, organization, projectSlug, identity string, enabled bool) (Observed, error) {
	target, err := m.adapter(adapter)
	if err != nil {
		return Observed{}, err
	}
	return target.SetEnabled(ctx, adapter, organization, projectSlug, identity, enabled)
}

func (m *MuxAdapter) Delete(ctx context.Context, adapter, organization, projectSlug, identity string, marker binding.Marker) error {
	target, err := m.adapter(adapter)
	if err != nil {
		return err
	}
	return target.Delete(ctx, adapter, organization, projectSlug, identity, marker)
}
