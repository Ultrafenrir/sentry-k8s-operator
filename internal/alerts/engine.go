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

// Package alerts implements the adapter-neutral protected-state lifecycle for Sentry alert
// children. It intentionally does not publish a CRD surface until target adapters have live
// lifecycle acceptance.
package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

type Trigger string

const (
	TriggerFirstSeen  Trigger = "first-seen"
	TriggerRegression Trigger = "regression"
	TriggerEventCount Trigger = "event-count"

	StateReady   = "Ready"
	StateDeleted = "Deleted"
	StateError   = "Error"
)

var (
	ErrInvalidInput       = errors.New("alert desired state is invalid")
	ErrAdapterUnavailable = errors.New("pinned alert adapter is unavailable or incompatible")
	ErrUnsupportedUpdate  = errors.New("alert adapter cannot safely update this rule")
	ErrExternalMissing    = errors.New("bound external alert is missing")
	ErrOwnershipLost      = errors.New("bound external alert ownership evidence is missing")
)

var localKeyPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)

// Desired is deliberately adapter-neutral. It is internal until the same lifecycle is
// accepted against every advertised Sentry target.
type Desired struct {
	Trigger       Trigger
	WindowMinutes int
	Threshold     int64
	Enabled       bool
	Notify        []string
}

type Capability struct {
	Name           string
	Triggers       map[Trigger]bool
	SupportsUpdate bool
	SupportsToggle bool
}

type Observed struct {
	Identity    string
	Adapter     string
	Marker      binding.Marker
	DesiredHash string
	Enabled     bool
}

// Adapter methods operate on exact protected identities. Ensure must be idempotent for the
// tuple (project, localKey, marker), so a lost create response cannot cause a second child.
type Adapter interface {
	Ensure(context.Context, string, string, string, string, Desired, binding.Marker) (Observed, error)
	Observe(context.Context, string, string, string, string) (Observed, bool, error)
	Update(context.Context, string, string, string, string, Desired) (Observed, error)
	SetEnabled(context.Context, string, string, string, string, bool) (Observed, error)
	Delete(context.Context, string, string, string, string, binding.Marker) error
}

type Authorization func(context.Context) error

type Input struct {
	Organization string
	ProjectSlug  string
	Manage       bool
	Desired      map[string]Desired
	Current      map[string]binding.AlertBinding
	Tombstones   map[string]bool
	Destinations map[string]binding.DestinationBinding
	// DestinationCleanup lists local destination keys whose external cleanup is waiting
	// for alert dependency release in the same generation.
	DestinationCleanup []string
	PinnedAdapter      string
	Capabilities       []Capability
	Marker             binding.Marker
	Authorize          Authorization
}

type Status struct {
	State  string
	Reason string
	Err    error
}

type Result struct {
	Bindings      map[string]binding.AlertBinding
	Tombstones    map[string]bool
	PinnedAdapter string
	Statuses      map[string]Status
	// ReleasableDestinations contains only requested cleanup keys with no desired or
	// still-bound managed alert references after this reconciliation.
	ReleasableDestinations []string
}

func (r Result) Err() error {
	errs := make([]error, 0, len(r.Statuses))
	for _, status := range r.Statuses {
		if status.Err != nil {
			errs = append(errs, status.Err)
		}
	}
	return errors.Join(errs...)
}

type Engine struct {
	Adapter Adapter
	Journal sentryapi.OperationJournal
	Audit   audit.Sink
}

func (e *Engine) Reconcile(ctx context.Context, input Input) (result Result) {
	result = Result{
		Bindings:      cloneBindings(input.Current),
		Tombstones:    cloneTombstones(input.Tombstones),
		PinnedAdapter: input.PinnedAdapter,
		Statuses:      make(map[string]Status),
	}
	defer func() {
		if input.Manage {
			result.ReleasableDestinations = releasableDestinations(input, result.Bindings)
		}
	}()
	if !input.Manage {
		return result
	}
	if e.Adapter == nil || e.Journal == nil || e.Audit == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" || !input.Marker.Matches(input.Marker) {
		result.Statuses[""] = Status{State: StateError, Reason: "InvalidAlertInput", Err: ErrInvalidInput}
		return result
	}

	// Deletions are planned first so same-generation destination cleanup can only run after
	// every referencing managed alert has been removed.
	for _, key := range removedKeys(input.Current, input.Desired) {
		current := input.Current[key]
		if err := e.deleteOne(ctx, input, key, current); err != nil {
			result.Statuses[key] = Status{State: StateError, Reason: reasonFor(err), Err: err}
			continue
		}
		delete(result.Bindings, key)
		result.Tombstones[key] = true
		result.Statuses[key] = Status{State: StateDeleted, Reason: "AlertDeleted"}
	}

	desiredKeys := sortedDesiredKeys(input.Desired)
	if len(desiredKeys) == 0 {
		return result
	}
	validKeys := make([]string, 0, len(desiredKeys))
	for _, key := range desiredKeys {
		desired := normalize(input.Desired[key])
		if err := validateDesired(key, desired, input.Destinations); err != nil {
			result.Statuses[key] = Status{State: StateError, Reason: "InvalidAlert", Err: err}
			continue
		}
		validKeys = append(validKeys, key)
	}
	if len(validKeys) == 0 {
		return result
	}
	capability, err := selectCapability(input, validKeys)
	if err != nil {
		for _, key := range validKeys {
			result.Statuses[key] = Status{State: StateError, Reason: "AlertAdapterUnavailable", Err: err}
		}
		return result
	}
	result.PinnedAdapter = capability.Name

	for _, key := range validKeys {
		desired := normalize(input.Desired[key])
		hash := desiredHash(desired)
		current, found := input.Current[key]
		var next binding.AlertBinding
		if !found {
			next, err = e.ensureOne(ctx, input, capability, key, desired, hash)
		} else {
			next, err = e.updateOne(ctx, input, capability, key, current, desired, hash)
		}
		if err != nil {
			result.Statuses[key] = Status{State: StateError, Reason: reasonFor(err), Err: err}
			continue
		}
		result.Bindings[key] = next
		delete(result.Tombstones, key)
		result.Statuses[key] = Status{State: StateReady, Reason: "AlertReady"}
	}
	return result
}

func (e *Engine) ensureOne(ctx context.Context, input Input, capability Capability, key string, desired Desired, hash string) (binding.AlertBinding, error) {
	var observed Observed
	err := runMutation(ctx, e.Journal, mutation{
		Key: operationKey("ensure", input, key), Kind: "EnsureAlert",
		LocalResource: input.ProjectSlug + "/" + key, Hash: hash,
		Audit: e.Audit, Action: audit.ActionEnsure, Authorize: input.Authorize,
		Apply: func(ctx context.Context) error {
			var err error
			observed, err = e.Adapter.Ensure(ctx, capability.Name, input.Organization, input.ProjectSlug, key, desired, input.Marker)
			return err
		},
		Verify: func(context.Context) (bool, error) {
			if observed.Identity == "" {
				return false, nil
			}
			return verifyObserved(observed, capability.Name, input.Marker, hash, desired.Enabled) == nil, nil
		},
	})
	if err != nil {
		return binding.AlertBinding{}, err
	}
	if err := verifyObserved(observed, capability.Name, input.Marker, hash, desired.Enabled); err != nil {
		return binding.AlertBinding{}, err
	}
	return newBinding(observed, desired), nil
}

func (e *Engine) updateOne(ctx context.Context, input Input, capability Capability, key string, current binding.AlertBinding, desired Desired, hash string) (binding.AlertBinding, error) {
	if current.Adapter != capability.Name || current.ExternalIdentity == "" || !current.Marker.Matches(input.Marker) {
		return binding.AlertBinding{}, ErrOwnershipLost
	}
	observed, found, err := e.Adapter.Observe(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity)
	if err != nil {
		return binding.AlertBinding{}, err
	}
	if !found {
		return binding.AlertBinding{}, ErrExternalMissing
	}
	if err := verifyIdentity(observed, current, input.Marker); err != nil {
		return binding.AlertBinding{}, err
	}
	updateKey := operationKey("update", input, key)
	updatePending, err := pendingMutation(ctx, e.Journal, updateKey, hash)
	if err != nil {
		return binding.AlertBinding{}, err
	}
	if observed.DesiredHash != hash || updatePending {
		if !capability.SupportsUpdate {
			return binding.AlertBinding{}, ErrUnsupportedUpdate
		}
		err = runMutation(ctx, e.Journal, mutation{
			Key: updateKey, Kind: "UpdateAlert", LocalResource: input.ProjectSlug + "/" + key,
			Hash: hash, Audit: e.Audit, Action: audit.ActionUpdate, Authorize: input.Authorize,
			Apply: func(ctx context.Context) error {
				var applyErr error
				observed, applyErr = e.Adapter.Update(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity, desired)
				return applyErr
			},
			Verify: func(ctx context.Context) (bool, error) {
				fresh, found, verifyErr := e.Adapter.Observe(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity)
				if verifyErr != nil || !found {
					return false, verifyErr
				}
				if verifyErr = verifyIdentity(fresh, current, input.Marker); verifyErr != nil {
					return false, verifyErr
				}
				observed = fresh
				return fresh.DesiredHash == hash, nil
			},
		})
		if err != nil {
			return binding.AlertBinding{}, err
		}
	}
	toggleHash := mutationHash(hash, boolString(desired.Enabled))
	toggleKey := operationKey("toggle", input, key)
	togglePending, err := pendingMutation(ctx, e.Journal, toggleKey, toggleHash)
	if err != nil {
		return binding.AlertBinding{}, err
	}
	if observed.Enabled != desired.Enabled || togglePending {
		if !capability.SupportsToggle {
			return binding.AlertBinding{}, ErrAdapterUnavailable
		}
		err = runMutation(ctx, e.Journal, mutation{
			Key: toggleKey, Kind: "ToggleAlert", LocalResource: input.ProjectSlug + "/" + key,
			Hash: toggleHash, Audit: e.Audit, Action: audit.ActionToggle, Authorize: input.Authorize,
			Apply: func(ctx context.Context) error {
				var applyErr error
				observed, applyErr = e.Adapter.SetEnabled(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity, desired.Enabled)
				return applyErr
			},
			Verify: func(ctx context.Context) (bool, error) {
				fresh, found, verifyErr := e.Adapter.Observe(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity)
				if verifyErr != nil || !found {
					return false, verifyErr
				}
				if verifyErr = verifyIdentity(fresh, current, input.Marker); verifyErr != nil {
					return false, verifyErr
				}
				observed = fresh
				return fresh.Enabled == desired.Enabled, nil
			},
		})
		if err != nil {
			return binding.AlertBinding{}, err
		}
	}
	if err := verifyObserved(observed, capability.Name, input.Marker, hash, desired.Enabled); err != nil {
		return binding.AlertBinding{}, err
	}
	return newBinding(observed, desired), nil
}

func (e *Engine) deleteOne(ctx context.Context, input Input, key string, current binding.AlertBinding) error {
	if current.ExternalIdentity == "" || current.Adapter == "" || !current.Marker.Matches(input.Marker) {
		return ErrOwnershipLost
	}
	return runMutation(ctx, e.Journal, mutation{
		Key: operationKey("delete", input, key), Kind: "DeleteAlert", LocalResource: input.ProjectSlug + "/" + key,
		Hash:  mutationHash(current.Adapter, current.ExternalIdentity, current.DesiredHash),
		Audit: e.Audit, Action: audit.ActionDelete, Authorize: input.Authorize,
		Apply: func(ctx context.Context) error {
			return e.Adapter.Delete(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity, current.Marker)
		},
		Verify: func(ctx context.Context) (bool, error) {
			observed, found, verifyErr := e.Adapter.Observe(ctx, current.Adapter, input.Organization, input.ProjectSlug, current.ExternalIdentity)
			if verifyErr != nil || !found {
				return !found, verifyErr
			}
			if verifyErr = verifyIdentity(observed, current, input.Marker); verifyErr != nil {
				return false, verifyErr
			}
			return false, nil
		},
	})
}

func selectCapability(input Input, keys []string) (Capability, error) {
	candidates := append([]Capability(nil), input.Capabilities...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	pinned := input.PinnedAdapter
	if pinned == "" {
		for _, key := range keys {
			if current, ok := input.Current[key]; ok {
				if pinned != "" && pinned != current.Adapter {
					return Capability{}, ErrAdapterUnavailable
				}
				pinned = current.Adapter
			}
		}
	}
	for _, candidate := range candidates {
		if pinned != "" && candidate.Name != pinned {
			continue
		}
		if !candidate.SupportsToggle {
			continue
		}
		compatible := true
		for _, key := range keys {
			if !candidate.Triggers[input.Desired[key].Trigger] {
				compatible = false
				break
			}
		}
		if compatible {
			return candidate, nil
		}
	}
	return Capability{}, ErrAdapterUnavailable
}

func validateDesired(key string, desired Desired, destinations map[string]binding.DestinationBinding) error {
	if len(key) > 63 || !localKeyPattern.MatchString(key) || len(desired.Notify) == 0 || len(desired.Notify) > 20 {
		return ErrInvalidInput
	}
	switch desired.Trigger {
	case TriggerFirstSeen, TriggerRegression:
		if desired.WindowMinutes != 0 || desired.Threshold != 0 {
			return ErrInvalidInput
		}
	case TriggerEventCount:
		if desired.WindowMinutes < 1 || desired.WindowMinutes > 1440 || desired.Threshold <= 0 {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(desired.Notify))
	for _, name := range desired.Notify {
		if _, duplicate := seen[name]; duplicate {
			return ErrInvalidInput
		}
		seen[name] = struct{}{}
		destination, ok := destinations[name]
		if !ok || destination.State != "Active" {
			return ErrInvalidInput
		}
	}
	return nil
}

func normalize(desired Desired) Desired {
	desired.Notify = append([]string(nil), desired.Notify...)
	sort.Strings(desired.Notify)
	return desired
}

func desiredHash(desired Desired) string {
	payload, _ := json.Marshal(struct {
		Trigger       Trigger  `json:"trigger"`
		WindowMinutes int      `json:"windowMinutes,omitempty"`
		Threshold     int64    `json:"threshold,omitempty"`
		Notify        []string `json:"notify"`
	}{
		Trigger: desired.Trigger, WindowMinutes: desired.WindowMinutes,
		Threshold: desired.Threshold, Notify: desired.Notify,
	})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func verifyIdentity(observed Observed, current binding.AlertBinding, marker binding.Marker) error {
	if observed.Identity != current.ExternalIdentity || observed.Adapter != current.Adapter || !observed.Marker.Matches(marker) {
		return ErrOwnershipLost
	}
	return nil
}

func verifyObserved(observed Observed, adapter string, marker binding.Marker, hash string, enabled bool) error {
	if observed.Identity == "" || observed.Adapter != adapter || !observed.Marker.Matches(marker) {
		return ErrOwnershipLost
	}
	if observed.DesiredHash != hash || observed.Enabled != enabled {
		return ErrInvalidInput
	}
	return nil
}

func newBinding(observed Observed, desired Desired) binding.AlertBinding {
	return binding.AlertBinding{
		Adapter: observed.Adapter, ExternalIdentity: observed.Identity, Trigger: string(desired.Trigger),
		DesiredHash: observed.DesiredHash, Enabled: observed.Enabled,
		Destinations: append([]string(nil), desired.Notify...), Marker: observed.Marker,
	}
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, ErrOwnershipLost):
		return "AlertOwnershipLost"
	case errors.Is(err, ErrExternalMissing):
		return "AlertMissing"
	case errors.Is(err, ErrUnsupportedUpdate):
		return "AlertUpdateUnsupported"
	case errors.Is(err, ErrAdapterUnavailable):
		return "AlertAdapterUnavailable"
	case errors.Is(err, ErrInvalidInput):
		return "InvalidAlert"
	default:
		return "AlertOperationFailed"
	}
}

func sortedDesiredKeys(desired map[string]Desired) []string {
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func removedKeys(current map[string]binding.AlertBinding, desired map[string]Desired) []string {
	keys := make([]string, 0, len(current))
	for key := range current {
		if _, ok := desired[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func cloneBindings(source map[string]binding.AlertBinding) map[string]binding.AlertBinding {
	result := make(map[string]binding.AlertBinding, len(source))
	for key, value := range source {
		value.Destinations = append([]string(nil), value.Destinations...)
		result[key] = value
	}
	return result
}

func cloneTombstones(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func releasableDestinations(input Input, remaining map[string]binding.AlertBinding) []string {
	referenced := make(map[string]bool)
	for _, desired := range input.Desired {
		for _, name := range desired.Notify {
			referenced[name] = true
		}
	}
	for _, current := range remaining {
		for _, name := range current.Destinations {
			referenced[name] = true
		}
	}
	requested := make(map[string]bool, len(input.DestinationCleanup))
	for _, name := range input.DestinationCleanup {
		if name != "" && !referenced[name] {
			requested[name] = true
		}
	}
	result := make([]string, 0, len(requested))
	for name := range requested {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func operationKey(kind string, input Input, key string) string {
	return "alerts:" + kind + ":" + input.Organization + ":" + input.ProjectSlug + ":" + key
}

func mutationHash(values ...string) string {
	payload, _ := json.Marshal(values)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
