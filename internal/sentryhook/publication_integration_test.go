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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPublicationExactCleanupAgainstAPIServer(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest API server: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest API server: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, name := range []string{"publication-system", "publication-sources"} {
		if err := kubernetesClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	siblingKey := strings.Repeat("f", 64) + ".json"
	aggregate := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "publication-system", Name: DefaultVerifierSecretName}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{siblingKey: []byte("opaque-admin-sibling")},
	}
	if err := kubernetesClient.Create(ctx, aggregate); err != nil {
		t.Fatal(err)
	}
	verifierStore := &VerifierStore{Client: kubernetesClient, Namespace: "publication-system"}
	publisher := &Publisher{
		Sources:  &SourceStore{Client: kubernetesClient, Namespace: "publication-sources"},
		Verifier: verifierStore,
	}
	source := strings.Repeat("a", 64)
	plan := SourcePlan{SourceBindingKey: source, ExpectedProjectSlug: "checkout"}
	oldPayload, _ := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-source-secret")})
	record, err := publisher.Publish(ctx, plan, oldPayload)
	if err != nil || record.ResourceVersion == "" {
		t.Fatalf("publish record=%#v error=%v", record, err)
	}

	// Fail external verification after publishing overlap. The real API server retains the
	// safe partial state, stale cleanup cannot remove protected routing, and a normal retry
	// completes the exact same rotation.
	newSecret := []byte("new-source-secret")
	overlapPayload, _ := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-source-secret"), newSecret})
	switcher := &rotationSwitcherFake{expectedSecret: newSecret, fail: true}
	rotator := &VerifierRotator{Store: verifierStore, Switcher: switcher}
	if _, err := rotator.Rotate(ctx, source, newSecret); !errors.Is(err, ErrVerifierRotation) {
		t.Fatalf("injected verification failure error=%v", err)
	}
	currentPayload, err := verifierStore.Get(ctx, source)
	if err != nil || string(currentPayload) != string(overlapPayload) {
		t.Fatalf("overlap payload=%q error=%v", currentPayload, err)
	}
	if err := publisher.Unpublish(ctx, record, oldPayload); !errors.Is(err, ErrPublication) {
		t.Fatalf("stale unpublish error=%v", err)
	}
	if _, err := publisher.Sources.Get(ctx, source); err != nil {
		t.Fatalf("stale cleanup removed protected source: %v", err)
	}
	switcher.fail = false
	retiredPayload, err := rotator.Rotate(ctx, source, newSecret)
	if err != nil || switcher.calls != 2 {
		t.Fatalf("rotation resume payload=%q calls=%d error=%v", retiredPayload, switcher.calls, err)
	}
	if err := publisher.Unpublish(ctx, record, overlapPayload); !errors.Is(err, ErrPublication) {
		t.Fatalf("retired overlap stale cleanup error=%v", err)
	}
	if err := publisher.Unpublish(ctx, record, retiredPayload); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Sources.Get(ctx, source); !apierrors.IsNotFound(err) {
		t.Fatalf("source remains after exact cleanup: %v", err)
	}
	current := &corev1.Secret{}
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: "publication-system", Name: DefaultVerifierSecretName}, current); err != nil {
		t.Fatal(err)
	}
	if string(current.Data[siblingKey]) != "opaque-admin-sibling" {
		t.Fatalf("sibling changed during publication round trip: %#v", current.Data)
	}
	if _, found := current.Data[verifierDataKey(source)]; found {
		t.Fatal("source verifier entry remains after exact cleanup")
	}
}
