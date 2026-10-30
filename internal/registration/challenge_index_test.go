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

package registration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

func TestChallengeIndexUsesExactImmutableRecordAndDeletesOnlyExpectedValue(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &ChallengeIndexStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state"}
	record := ChallengeIndex{
		Version: binding.CurrentVersion, CodeHash: strings.Repeat("a", 64), ProjectUID: "project-uid",
		Destination: "telegram", ExpiresAt: time.Date(2026, 10, 29, 22, 10, 0, 0, time.UTC),
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), record.CodeHash)
	if err != nil || got != record {
		t.Fatalf("record=%#v err=%v", got, err)
	}
	wrong := record
	wrong.ProjectUID = "other"
	if err := store.DeleteExact(context.Background(), wrong); !errors.Is(err, ErrChallengeIndexInvalid) {
		t.Fatalf("wrong exact delete=%v", err)
	}
	if err := store.DeleteExact(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteExact(context.Background(), record); err != nil {
		t.Fatalf("idempotent delete=%v", err)
	}
	if _, err := store.Get(context.Background(), record.CodeHash); !apierrors.IsNotFound(err) {
		t.Fatalf("get after delete=%v", err)
	}
}

func TestChallengeIndexRejectsMalformedRecords(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &ChallengeIndexStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state"}
	for name, record := range map[string]ChallengeIndex{
		"hash":        {Version: binding.CurrentVersion, CodeHash: "bad", ProjectUID: "p", Destination: "d", ExpiresAt: time.Now()},
		"project":     {Version: binding.CurrentVersion, CodeHash: strings.Repeat("b", 64), Destination: "d", ExpiresAt: time.Now()},
		"destination": {Version: binding.CurrentVersion, CodeHash: strings.Repeat("c", 64), ProjectUID: "p", ExpiresAt: time.Now()},
		"expiry":      {Version: binding.CurrentVersion, CodeHash: strings.Repeat("d", 64), ProjectUID: "p", Destination: "d"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.Create(context.Background(), record); !errors.Is(err, ErrChallengeIndexInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestChallengeIndexSweepRemovesOldestExpiredRecordsOnly(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &ChallengeIndexStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	early := testChallengeIndex("a", now.Add(-time.Hour))
	late := testChallengeIndex("b", now)
	future := testChallengeIndex("c", now.Add(time.Hour))
	for _, record := range []ChallengeIndex{late, future, early} {
		if err := store.Create(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.SweepExpired(context.Background(), now, 1)
	if err != nil || removed != 1 {
		t.Fatalf("first sweep = (%d, %v)", removed, err)
	}
	if _, err := store.Get(context.Background(), early.CodeHash); !apierrors.IsNotFound(err) {
		t.Fatalf("oldest expired record remains: %v", err)
	}
	if _, err := store.Get(context.Background(), late.CodeHash); err != nil {
		t.Fatalf("limited expired record was removed: %v", err)
	}
	removed, err = store.SweepExpired(context.Background(), now, 10)
	if err != nil || removed != 1 {
		t.Fatalf("second sweep = (%d, %v)", removed, err)
	}
	if _, err := store.Get(context.Background(), future.CodeHash); err != nil {
		t.Fatalf("future record was removed: %v", err)
	}
}

func TestChallengeIndexSweepKeepsMalformedRecordButContinues(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := &ChallengeIndexStore{Client: client, Namespace: "state"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	valid := testChallengeIndex("d", now.Add(-time.Minute))
	if err := store.Create(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	immutable := true
	malformed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "malformed", Labels: map[string]string{recordTypeLabel: challengeIndexLabel}},
		Immutable:  &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{challengeIndexKey: []byte("not-json")},
	}
	if err := client.Create(context.Background(), malformed); err != nil {
		t.Fatal(err)
	}
	removed, err := store.SweepExpired(context.Background(), now, 10)
	if removed != 1 || !errors.Is(err, ErrChallengeIndexInvalid) {
		t.Fatalf("sweep = (%d, %v)", removed, err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "state", Name: "malformed"}, &corev1.Secret{}); err != nil {
		t.Fatalf("malformed record was deleted: %v", err)
	}
}

func testChallengeIndex(character string, expiry time.Time) ChallengeIndex {
	return ChallengeIndex{
		Version: binding.CurrentVersion, CodeHash: strings.Repeat(character, 64), ProjectUID: "project-uid",
		Destination: "telegram", ExpiresAt: expiry,
	}
}
