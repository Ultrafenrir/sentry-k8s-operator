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

package clientkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

const (
	StateReady           = "Ready"
	StateRotationPending = "RotationPending"
	StateDeleted         = "Deleted"
	StateError           = "Error"
)

var (
	ErrInvalidInput     = errors.New("managed client-key desired state is invalid")
	ErrOwnershipLost    = errors.New("managed client-key ownership evidence is missing")
	ErrExternalMissing  = errors.New("managed client-key external object is missing")
	ErrRecoveryPending  = errors.New("managed client-key mutation recovery is pending")
	ErrRotationRollback = errors.New("managed client-key rotation revision cannot decrease")
	ErrVerification     = errors.New("managed client-key mutation verification failed")
	localKeyPattern     = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)
)

type Desired struct {
	Active           bool
	RateLimit        *RateLimit
	RotationRevision int64
}

type Authorization func(context.Context) error

type Input struct {
	Organization string
	ProjectSlug  string
	ProjectUID   string
	Desired      map[string]Desired
	Current      map[string]binding.ClientKeyBinding
	// Promote confirms that a replacement public DSN has completed its Kubernetes Secret
	// cutover, or that the key is not selected for DSN publication.
	Promote   map[string]bool
	Authorize Authorization
}

type Status struct {
	State            string
	Reason           string
	RotationRevision int64
	Err              error
}

type Result struct {
	Bindings map[string]binding.ClientKeyBinding
	Statuses map[string]Status
	// PublicDSNs is transient reconciliation output. It must never enter protected binding,
	// public status, Events, metrics, or logs.
	PublicDSNs map[string]string
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
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{
		Bindings:   cloneBindings(input.Current),
		Statuses:   make(map[string]Status),
		PublicDSNs: make(map[string]string),
	}
	if e == nil || e.Adapter == nil || e.Journal == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" || input.ProjectUID == "" {
		result.Statuses[""] = Status{State: StateError, Reason: "InvalidClientKeyInput", Err: ErrInvalidInput}
		return result
	}

	for _, key := range removedKeys(input.Current, input.Desired) {
		current := input.Current[key]
		if err := e.deleteBinding(ctx, input, key, current); err != nil {
			result.Statuses[key] = errorStatus(current.RotationRevision, err)
			continue
		}
		delete(result.Bindings, key)
		result.Statuses[key] = Status{State: StateDeleted, Reason: "ClientKeyDeleted", RotationRevision: current.RotationRevision}
	}

	for _, key := range sortedDesiredKeys(input.Desired) {
		desired := input.Desired[key]
		if err := validateDesired(key, desired); err != nil {
			result.Statuses[key] = errorStatus(desired.RotationRevision, err)
			continue
		}
		current, found := input.Current[key]
		var next binding.ClientKeyBinding
		var remote Remote
		var err error
		if !found {
			next, remote, err = e.createOne(ctx, input, key, desired)
		} else {
			next, remote, err = e.reconcileOne(ctx, input, key, desired, current)
		}
		if err != nil {
			result.Statuses[key] = errorStatus(desired.RotationRevision, err)
			continue
		}
		result.Bindings[key] = next
		if remote.IsActive && remote.DSN.Public != "" {
			result.PublicDSNs[key] = remote.DSN.Public
		}
		if next.ReplacementIdentity != "" {
			result.Statuses[key] = Status{State: StateRotationPending, Reason: "ClientKeyCutoverPending", RotationRevision: desired.RotationRevision}
		} else {
			result.Statuses[key] = Status{State: StateReady, Reason: "ClientKeyReady", RotationRevision: desired.RotationRevision}
		}
	}
	return result
}

func (e *Engine) createOne(ctx context.Context, input Input, key string, desired Desired) (binding.ClientKeyBinding, Remote, error) {
	name := remoteName(key, input.ProjectUID, desired.RotationRevision)
	remote, err := e.ensureCreated(ctx, input, key, name, desired)
	if err != nil {
		return binding.ClientKeyBinding{}, Remote{}, err
	}
	return newBinding(remote, desired), remote, nil
}

func (e *Engine) reconcileOne(ctx context.Context, input Input, key string, desired Desired, current binding.ClientKeyBinding) (binding.ClientKeyBinding, Remote, error) {
	if desired.RotationRevision < current.RotationRevision {
		return binding.ClientKeyBinding{}, Remote{}, ErrRotationRollback
	}
	if current.ExternalIdentity == "" || current.RemoteName != remoteName(key, input.ProjectUID, current.RotationRevision) {
		return binding.ClientKeyBinding{}, Remote{}, ErrOwnershipLost
	}
	if desired.RotationRevision > current.RotationRevision || current.ReplacementIdentity != "" {
		return e.reconcileRotation(ctx, input, key, desired, current)
	}
	remote, err := e.observeExact(ctx, input, current.ExternalIdentity, current.RemoteName)
	if err != nil {
		return binding.ClientKeyBinding{}, Remote{}, err
	}
	remote, err = e.ensureConfiguration(ctx, input, remote, desired)
	if err != nil {
		return binding.ClientKeyBinding{}, Remote{}, err
	}
	return newBinding(remote, desired), remote, nil
}

func (e *Engine) reconcileRotation(ctx context.Context, input Input, key string, desired Desired, current binding.ClientKeyBinding) (binding.ClientKeyBinding, Remote, error) {
	if current.ReplacementIdentity != "" && current.ReplacementRevision != desired.RotationRevision {
		return binding.ClientKeyBinding{}, Remote{}, ErrRecoveryPending
	}
	name := remoteName(key, input.ProjectUID, desired.RotationRevision)
	replacement, err := e.ensureCreated(ctx, input, key, name, desired)
	if err != nil {
		return binding.ClientKeyBinding{}, Remote{}, err
	}
	next := current
	next.ReplacementIdentity = replacement.Identity
	next.ReplacementRemoteName = replacement.Name
	next.ReplacementRevision = desired.RotationRevision
	next.ReplacementDesiredHash = desiredHash(desired)
	if !input.Promote[key] {
		return next, replacement, nil
	}
	if err := e.deleteExact(ctx, input, key, current.ExternalIdentity, current.RemoteName, current.RotationRevision); err != nil {
		return next, replacement, err
	}
	return newBinding(replacement, desired), replacement, nil
}

func (e *Engine) ensureCreated(ctx context.Context, input Input, key, name string, desired Desired) (Remote, error) {
	var observed Remote
	verify := func(ctx context.Context) (bool, error) {
		items, err := e.Adapter.List(ctx, input.Organization, input.ProjectSlug)
		if err != nil {
			return false, err
		}
		matches := make([]Remote, 0, 1)
		for _, item := range items {
			if item.Name == name {
				matches = append(matches, item)
			}
		}
		if len(matches) > 1 {
			return false, ErrOwnershipLost
		}
		if len(matches) == 0 {
			return false, nil
		}
		observed = matches[0]
		return observed.Identity != "", nil
	}
	err := e.runCreate(ctx, input, key, name, desired, func(ctx context.Context) error {
		var err error
		observed, err = e.Adapter.Create(ctx, input.Organization, input.ProjectSlug, CreateRequest{Name: name, RateLimit: cloneRateLimit(desired.RateLimit)})
		return err
	}, verify)
	if err != nil {
		return Remote{}, err
	}
	if observed.Identity == "" || observed.Name != name {
		return Remote{}, ErrVerification
	}
	return e.ensureConfiguration(ctx, input, observed, desired)
}

func (e *Engine) ensureConfiguration(ctx context.Context, input Input, remote Remote, desired Desired) (Remote, error) {
	if remote.Identity == "" || remote.Name == "" {
		return Remote{}, ErrOwnershipLost
	}
	if remote.IsActive == desired.Active && equalRateLimit(remote.RateLimit, desired.RateLimit) {
		return remote, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return Remote{}, err
	}
	updated, err := e.Adapter.Update(ctx, input.Organization, input.ProjectSlug, remote.Identity, UpdateRequest{
		Name: remote.Name, IsActive: desired.Active, RateLimit: cloneRateLimit(desired.RateLimit),
	})
	if err != nil {
		return Remote{}, err
	}
	observed, err := e.observeExact(ctx, input, remote.Identity, remote.Name)
	if err != nil {
		return Remote{}, err
	}
	if updated.Identity != remote.Identity || observed.IsActive != desired.Active || !equalRateLimit(observed.RateLimit, desired.RateLimit) {
		return Remote{}, ErrVerification
	}
	return observed, nil
}

func (e *Engine) observeExact(ctx context.Context, input Input, identity, name string) (Remote, error) {
	remote, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, identity)
	if err != nil {
		return Remote{}, err
	}
	if !found {
		return Remote{}, ErrExternalMissing
	}
	if remote.Identity != identity || remote.Name != name {
		return Remote{}, ErrOwnershipLost
	}
	return remote, nil
}

func (e *Engine) deleteBinding(ctx context.Context, input Input, key string, current binding.ClientKeyBinding) error {
	if current.ReplacementIdentity != "" {
		if err := e.deleteExact(ctx, input, key, current.ReplacementIdentity, current.ReplacementRemoteName, current.ReplacementRevision); err != nil {
			return err
		}
	}
	return e.deleteExact(ctx, input, key, current.ExternalIdentity, current.RemoteName, current.RotationRevision)
}

func (e *Engine) deleteExact(ctx context.Context, input Input, key, identity, name string, revision int64) error {
	if identity == "" || name != remoteName(key, input.ProjectUID, revision) {
		return ErrOwnershipLost
	}
	remote, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, identity)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if remote.Identity != identity || remote.Name != name {
		return ErrOwnershipLost
	}
	if err := input.Authorize(ctx); err != nil {
		return err
	}
	if err := e.Adapter.Delete(ctx, input.Organization, input.ProjectSlug, identity); err != nil {
		return err
	}
	_, found, err = e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, identity)
	if err != nil {
		return err
	}
	if found {
		return ErrVerification
	}
	return nil
}

func (e *Engine) runCreate(ctx context.Context, input Input, key, name string, desired Desired, apply func(context.Context) error, verify func(context.Context) (bool, error)) error {
	operationKey := "client-keys:create:" + input.Organization + ":" + input.ProjectSlug + ":" + key + ":" + strconv.FormatInt(desired.RotationRevision, 10)
	hash := operationHash(desired, name)
	current, found, err := e.Journal.Load(ctx, operationKey)
	if err != nil {
		return err
	}
	if found {
		if current.IdempotencyHash != hash {
			return ErrRecoveryPending
		}
		verified, verifyErr := verify(ctx)
		if verifyErr != nil {
			return verifyErr
		}
		if verified {
			if current.Phase == sentryapi.OperationPlanned {
				return ErrOwnershipLost
			}
			if current.Phase != sentryapi.OperationVerified {
				next := current
				next.Phase = sentryapi.OperationVerified
				next.Revision++
				if err := e.Journal.CompareAndSwap(ctx, current.Revision, next); err != nil {
					return err
				}
			}
			return nil
		}
		if current.Phase == sentryapi.OperationSubmitted {
			return ErrRecoveryPending
		}
		if current.Phase == sentryapi.OperationVerified {
			return ErrExternalMissing
		}
		if current.Phase != sentryapi.OperationPlanned {
			return ErrRecoveryPending
		}
		return e.submitCreate(ctx, input, current, apply, verify)
	}
	preexisting, err := verify(ctx)
	if err != nil {
		return err
	}
	if preexisting {
		// An exact readable label without this durable create journal is not sufficient
		// ownership evidence and must never be adopted by name alone.
		return ErrOwnershipLost
	}
	planned := sentryapi.Operation{Key: operationKey, Kind: "CreateClientKey", LocalResource: input.ProjectSlug + "/" + key, IdempotencyHash: hash, Phase: sentryapi.OperationPlanned, Revision: 1}
	if err := e.Journal.CompareAndSwap(ctx, 0, planned); err != nil {
		return err
	}
	return e.submitCreate(ctx, input, planned, apply, verify)
}

func (e *Engine) submitCreate(ctx context.Context, input Input, planned sentryapi.Operation, apply func(context.Context) error, verify func(context.Context) (bool, error)) error {
	if err := input.Authorize(ctx); err != nil {
		return err
	}
	submitted := planned
	submitted.Phase = sentryapi.OperationSubmitted
	submitted.Revision++
	if err := e.Journal.CompareAndSwap(ctx, planned.Revision, submitted); err != nil {
		return err
	}
	if err := apply(ctx); err != nil {
		return err
	}
	verified, err := verify(ctx)
	if err != nil {
		return err
	}
	if !verified {
		return ErrVerification
	}
	completed := submitted
	completed.Phase = sentryapi.OperationVerified
	completed.Revision++
	return e.Journal.CompareAndSwap(ctx, submitted.Revision, completed)
}

func validateDesired(key string, desired Desired) error {
	if len(key) > 32 || !localKeyPattern.MatchString(key) || desired.RotationRevision < 1 || desired.RotationRevision > 2147483647 {
		return ErrInvalidInput
	}
	if desired.RateLimit != nil && (desired.RateLimit.Count < 1 || desired.RateLimit.Count > 2147483647 || desired.RateLimit.Window < 1 || desired.RateLimit.Window > 86400) {
		return ErrInvalidInput
	}
	if len(remoteName(key, "uid", desired.RotationRevision)) > 64 {
		return ErrInvalidInput
	}
	return nil
}

func remoteName(key, projectUID string, revision int64) string {
	digest := sha256.Sum256([]byte(projectUID))
	return fmt.Sprintf("sko:%s:%s:r%d", key, hex.EncodeToString(digest[:6]), revision)
}

func desiredHash(desired Desired) string {
	payload, _ := json.Marshal(desired)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func operationHash(desired Desired, name string) string {
	digest := sha256.Sum256([]byte(desiredHash(desired) + "\x00" + name))
	return hex.EncodeToString(digest[:])
}

func newBinding(remote Remote, desired Desired) binding.ClientKeyBinding {
	return binding.ClientKeyBinding{
		ExternalIdentity: remote.Identity, RemoteName: remote.Name,
		RotationRevision: desired.RotationRevision, Active: desired.Active,
		RateLimit:   bindingRateLimit(desired.RateLimit),
		DesiredHash: desiredHash(desired),
	}
}

func bindingRateLimit(value *RateLimit) *binding.ClientKeyRateLimit {
	if value == nil {
		return nil
	}
	return &binding.ClientKeyRateLimit{Count: value.Count, WindowSeconds: value.Window}
}

func cloneRateLimit(value *RateLimit) *RateLimit {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func equalRateLimit(left, right *RateLimit) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func errorStatus(revision int64, err error) Status {
	reason := "ClientKeyOperationFailed"
	switch {
	case errors.Is(err, ErrInvalidInput):
		reason = "InvalidClientKey"
	case errors.Is(err, ErrOwnershipLost):
		reason = "ClientKeyOwnershipLost"
	case errors.Is(err, ErrExternalMissing):
		reason = "ClientKeyMissing"
	case errors.Is(err, ErrRecoveryPending):
		reason = "ClientKeyRecoveryPending"
	case errors.Is(err, ErrRotationRollback):
		reason = "ClientKeyRotationRollback"
	case errors.Is(err, ErrVerification):
		reason = "ClientKeyVerificationFailed"
	}
	return Status{State: StateError, Reason: reason, RotationRevision: revision, Err: err}
}

func sortedDesiredKeys(desired map[string]Desired) []string {
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func removedKeys(current map[string]binding.ClientKeyBinding, desired map[string]Desired) []string {
	keys := make([]string, 0, len(current))
	for key := range current {
		if _, found := desired[key]; !found {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func cloneBindings(source map[string]binding.ClientKeyBinding) map[string]binding.ClientKeyBinding {
	result := make(map[string]binding.ClientKeyBinding, len(source))
	for key, value := range source {
		if value.RateLimit != nil {
			rate := *value.RateLimit
			value.RateLimit = &rate
		}
		result[key] = value
	}
	return result
}
