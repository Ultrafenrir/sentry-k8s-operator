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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifierStoreUpdatesOneEntryAndDeletesOnlyExactPayload(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 64)
	sibling := strings.Repeat("b", 64) + ".json"
	store := &VerifierStore{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: DefaultVerifierSecretName}, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{sibling: []byte("admin-sibling")},
		}).Build(),
		Namespace: "system",
	}
	payload, err := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-source-secret"), []byte("new-source-secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(context.Background(), source, payload); err != nil {
		t.Fatal(err)
	}
	replacement, err := EncodeVerifierConfig("checkout", [][]byte{[]byte("replacement-secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwap(context.Background(), source, []byte(`{"expectedProjectSlug":"checkout","hmacSecretsBase64":["c3RhbGUtc291cmNlLXNlY3JldA=="]}`), replacement); !errors.Is(err, ErrVerifierConfig) {
		t.Fatalf("stale entry CAS error=%v", err)
	}
	loaded, err := store.Get(context.Background(), source)
	if err != nil || !bytes.Equal(loaded, payload) {
		t.Fatalf("loaded=%q error=%v", loaded, err)
	}
	loaded[0] ^= 1
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: "system", Name: DefaultVerifierSecretName}
	if err := store.Client.Get(context.Background(), key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[sibling]) != "admin-sibling" || string(secret.Data[verifierDataKey(source)]) != string(payload) {
		t.Fatalf("aggregate data changed unexpectedly: %#v", secret.Data)
	}
	if err := store.CompareAndSwap(context.Background(), source, payload, replacement); err != nil {
		t.Fatal(err)
	}
	payload = replacement
	changed, err := EncodeVerifierConfig("checkout", [][]byte{[]byte("new-source-secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(context.Background(), source, changed); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteExact(context.Background(), source, payload); !errors.Is(err, ErrVerifierConfig) {
		t.Fatalf("stale exact delete error=%v", err)
	}
	if err := store.DeleteExact(context.Background(), source, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), source); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted entry get error=%v", err)
	}
	if err := store.Client.Get(context.Background(), key, secret); err != nil || string(secret.Data[sibling]) != "admin-sibling" {
		t.Fatalf("sibling lost after delete: secret=%#v error=%v", secret.Data, err)
	}
}

func TestVerifierStoreFailsClosedAndNeverCreatesAdminBoundary(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	store := &VerifierStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "system"}
	payload, err := EncodeVerifierConfig("checkout", [][]byte{[]byte("valid-source-secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(context.Background(), strings.Repeat("c", 64), payload); !apierrors.IsNotFound(err) {
		t.Fatalf("missing admin Secret error=%v", err)
	}
	if _, err := store.Get(context.Background(), strings.Repeat("c", 64)); !apierrors.IsNotFound(err) {
		t.Fatalf("missing admin Secret get error=%v", err)
	}
	secrets := &corev1.SecretList{}
	if err := store.Client.List(context.Background(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("store created boundary: count=%d error=%v", len(secrets.Items), err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"expectedProjectSlug":"checkout","hmacSecretsBase64":[],"unknown":true}`),
		[]byte(`{"expectedProjectSlug":"External ID","hmacSecretsBase64":["dmFsaWQtc291cmNlLXNlY3JldA=="]}`),
	} {
		if err := ValidateVerifierConfig(invalid); !errors.Is(err, ErrVerifierConfig) {
			t.Fatalf("invalid payload error=%v", err)
		}
	}
}

func TestPlanVerifierRotationBoundsOverlapAndSupportsResumeOrRollback(t *testing.T) {
	t.Parallel()
	oldSecret := []byte("old-rotation-secret")
	newSecret := []byte("new-rotation-secret")
	thirdSecret := []byte("third-rotation-secret")
	current, err := EncodeVerifierConfig("checkout", [][]byte{oldSecret})
	if err != nil {
		t.Fatal(err)
	}
	overlap, retired, err := PlanVerifierRotation(current, newSecret)
	if err != nil {
		t.Fatal(err)
	}
	configuration, accepted, err := decodeVerifierConfig(overlap)
	if err != nil || configuration.ExpectedProjectSlug != "checkout" || len(accepted) != 2 || !bytes.Equal(accepted[0], oldSecret) || !bytes.Equal(accepted[1], newSecret) {
		t.Fatalf("overlap=%#v accepted=%q error=%v", configuration, accepted, err)
	}
	_, retiredSecrets, err := decodeVerifierConfig(retired)
	if err != nil || len(retiredSecrets) != 1 || !bytes.Equal(retiredSecrets[0], newSecret) {
		t.Fatalf("retired=%q error=%v", retiredSecrets, err)
	}
	resumedOverlap, resumedRetired, err := PlanVerifierRotation(overlap, newSecret)
	if err != nil || !bytes.Equal(resumedOverlap, overlap) || !bytes.Equal(resumedRetired, retired) {
		t.Fatalf("resume overlapStable=%t retiredStable=%t error=%v", bytes.Equal(resumedOverlap, overlap), bytes.Equal(resumedRetired, retired), err)
	}
	if _, _, err := PlanVerifierRotation(overlap, thirdSecret); !errors.Is(err, ErrVerifierConfig) {
		t.Fatalf("third concurrent key error=%v", err)
	}
	_, rollback, err := PlanVerifierRotation(overlap, oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	_, rollbackSecrets, _ := decodeVerifierConfig(rollback)
	if len(rollbackSecrets) != 1 || !bytes.Equal(rollbackSecrets[0], oldSecret) {
		t.Fatalf("rollback secrets=%q", rollbackSecrets)
	}
}
