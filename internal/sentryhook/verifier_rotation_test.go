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
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVerifierRotatorOrdersOverlapSwitchVerificationAndRetirement(t *testing.T) {
	t.Parallel()
	oldSecret := []byte("old-rotation-secret")
	newSecret := []byte("new-rotation-secret")
	current, _ := EncodeVerifierConfig("checkout", [][]byte{oldSecret})
	events := []string{}
	store := &rotationStoreFake{payload: current, events: &events}
	switcher := &rotationSwitcherFake{events: &events, expectedSecret: newSecret}
	rotator := &VerifierRotator{Store: store, Switcher: switcher}

	retired, err := rotator.Rotate(context.Background(), strings.Repeat("a", 64), newSecret)
	if err != nil || strings.Join(events, ",") != "get,cas-overlap,switch-and-verify,cas-retired" || !bytes.Equal(retired, store.payload) {
		t.Fatalf("events=%v retired=%q stored=%q error=%v", events, retired, store.payload, err)
	}
	_, secrets, err := decodeVerifierConfig(store.payload)
	if err != nil || len(secrets) != 1 || !bytes.Equal(secrets[0], newSecret) {
		t.Fatalf("retired secrets=%q error=%v", secrets, err)
	}
	newSecret[0] ^= 1
	if bytes.Equal(switcher.receivedSecret, newSecret) {
		t.Fatal("switcher retained caller-owned secret memory")
	}
}

func TestVerifierRotatorResumesSafePartialStates(t *testing.T) {
	t.Parallel()
	oldSecret := []byte("old-rotation-secret")
	newSecret := []byte("new-rotation-secret")
	current, _ := EncodeVerifierConfig("checkout", [][]byte{oldSecret})
	store := &rotationStoreFake{payload: current, failCASCall: 2}
	switcher := &rotationSwitcherFake{expectedSecret: newSecret}
	rotator := &VerifierRotator{Store: store, Switcher: switcher}

	if _, err := rotator.Rotate(context.Background(), strings.Repeat("b", 64), newSecret); !errors.Is(err, ErrVerifierRotation) {
		t.Fatalf("retirement failure error=%v", err)
	}
	_, overlapSecrets, err := decodeVerifierConfig(store.payload)
	if err != nil || len(overlapSecrets) != 2 {
		t.Fatalf("unsafe partial payload secrets=%q error=%v", overlapSecrets, err)
	}
	store.failCASCall = 0
	if _, err := rotator.Rotate(context.Background(), strings.Repeat("b", 64), newSecret); err != nil {
		t.Fatalf("resume error=%v", err)
	}
	_, retiredSecrets, _ := decodeVerifierConfig(store.payload)
	if len(retiredSecrets) != 1 || !bytes.Equal(retiredSecrets[0], newSecret) || switcher.calls != 2 {
		t.Fatalf("retired secrets=%q switch calls=%d", retiredSecrets, switcher.calls)
	}
}

func TestVerifierRotatorLeavesOverlapWhenExternalVerificationFails(t *testing.T) {
	t.Parallel()
	current, _ := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-rotation-secret")})
	store := &rotationStoreFake{payload: current}
	switcher := &rotationSwitcherFake{expectedSecret: []byte("new-rotation-secret"), fail: true}
	rotator := &VerifierRotator{Store: store, Switcher: switcher}
	if _, err := rotator.Rotate(context.Background(), strings.Repeat("c", 64), []byte("new-rotation-secret")); !errors.Is(err, ErrVerifierRotation) {
		t.Fatalf("verification failure error=%v", err)
	}
	_, secrets, _ := decodeVerifierConfig(store.payload)
	if len(secrets) != 2 || store.casCalls != 1 {
		t.Fatalf("verification failure state secrets=%q casCalls=%d", secrets, store.casCalls)
	}
}

func TestVerifierRotatorRejectsThirdConcurrentKeyBeforeMutation(t *testing.T) {
	t.Parallel()
	current, _ := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-rotation-secret"), []byte("new-rotation-secret")})
	store := &rotationStoreFake{payload: current}
	switcher := &rotationSwitcherFake{expectedSecret: []byte("third-rotation-secret")}
	rotator := &VerifierRotator{Store: store, Switcher: switcher}
	if _, err := rotator.Rotate(context.Background(), strings.Repeat("d", 64), []byte("third-rotation-secret")); !errors.Is(err, ErrVerifierRotation) {
		t.Fatalf("third key error=%v", err)
	}
	if store.casCalls != 0 || switcher.calls != 0 || !bytes.Equal(store.payload, current) {
		t.Fatalf("third key mutated state: cas=%d switch=%d", store.casCalls, switcher.calls)
	}
}

type rotationStoreFake struct {
	payload     []byte
	events      *[]string
	casCalls    int
	failCASCall int
}

func (f *rotationStoreFake) Get(context.Context, string) ([]byte, error) {
	f.event("get")
	return append([]byte(nil), f.payload...), nil
}

func (f *rotationStoreFake) CompareAndSwap(_ context.Context, _ string, previous, next []byte) error {
	f.casCalls++
	if !bytes.Equal(f.payload, previous) {
		return ErrVerifierConfig
	}
	_, secrets, _ := decodeVerifierConfig(next)
	if len(secrets) == 2 {
		f.event("cas-overlap")
	} else {
		f.event("cas-retired")
	}
	if f.failCASCall == f.casCalls {
		return errors.New("injected CAS failure")
	}
	f.payload = append([]byte(nil), next...)
	return nil
}

func (f *rotationStoreFake) event(value string) {
	if f.events != nil {
		*f.events = append(*f.events, value)
	}
}

type rotationSwitcherFake struct {
	events         *[]string
	expectedSecret []byte
	receivedSecret []byte
	calls          int
	fail           bool
}

func (f *rotationSwitcherFake) SwitchAndVerify(_ context.Context, _ string, secret []byte) error {
	f.calls++
	if f.events != nil {
		*f.events = append(*f.events, "switch-and-verify")
	}
	if !bytes.Equal(secret, f.expectedSecret) {
		return errors.New("unexpected desired secret")
	}
	f.receivedSecret = append([]byte(nil), secret...)
	if f.fail {
		return errors.New("injected external verification failure")
	}
	return nil
}
