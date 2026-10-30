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

package sentryhook

import (
	"context"
	"errors"
)

var ErrVerifierRotation = errors.New("Sentry hook verifier rotation failed")

type VerifierRotationStore interface {
	Get(context.Context, string) ([]byte, error)
	CompareAndSwap(context.Context, string, []byte, []byte) error
}

// ServiceHookSecretSwitcher must idempotently switch the external service hook to the
// supplied secret and return only after an independent read verifies that exact secret is
// active. Implementations must not include the secret or raw upstream errors in logs.
type ServiceHookSecretSwitcher interface {
	SwitchAndVerify(context.Context, string, []byte) error
}

// VerifierRotator maintains the safe ordering for HMAC rotation:
//
//  1. publish old+new locally;
//  2. switch and independently verify the external hook;
//  3. retire old locally.
//
// Every partial state is safe and resumable. Exact entry CAS prevents a stale reconciler
// from replacing a concurrent rotation, and a third concurrent key fails before any
// external call.
type VerifierRotator struct {
	Store    VerifierRotationStore
	Switcher ServiceHookSecretSwitcher
}

func (r *VerifierRotator) Rotate(ctx context.Context, sourceBindingKey string, desiredSecret []byte) ([]byte, error) {
	if r == nil || r.Store == nil || r.Switcher == nil || !canonicalHash(sourceBindingKey) {
		return nil, ErrVerifierRotation
	}
	current, err := r.Store.Get(ctx, sourceBindingKey)
	if err != nil {
		return nil, ErrVerifierRotation
	}
	overlap, retired, err := PlanVerifierRotation(current, desiredSecret)
	if err != nil {
		return nil, ErrVerifierRotation
	}
	if err := r.Store.CompareAndSwap(ctx, sourceBindingKey, current, overlap); err != nil {
		return nil, ErrVerifierRotation
	}
	if err := r.Switcher.SwitchAndVerify(ctx, sourceBindingKey, append([]byte(nil), desiredSecret...)); err != nil {
		return nil, ErrVerifierRotation
	}
	if err := r.Store.CompareAndSwap(ctx, sourceBindingKey, overlap, retired); err != nil {
		return nil, ErrVerifierRotation
	}
	return append([]byte(nil), retired...), nil
}

var _ VerifierRotationStore = (*VerifierStore)(nil)
