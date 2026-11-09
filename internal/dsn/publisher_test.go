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

package dsn

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

const testPublicDSN = "https://public@sentry.example/1"

func TestPublisherCreatesRestoresAndUpdatesOwnedSecret(t *testing.T) {
	t.Parallel()

	publisher, kubeClient := testPublisher(t)
	project := testProject()
	authorizations := 0
	authorize := func(context.Context) error { authorizations++; return nil }
	state := binding.DSNBinding{KeyIdentity: "key-id", KeyName: "Default", SelectorHash: "hash"}
	state, phase, err := publisher.Reconcile(context.Background(), project, "payments", "", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseReady || state.CurrentSecret != "checkout-sentry" || authorizations != 1 {
		t.Fatalf("initial publish state=%#v phase=%s auth=%d err=%v", state, phase, authorizations, err)
	}
	secret := &corev1.Secret{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: "checkout-sentry"}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 1 || string(secret.Data[sentryv1alpha1.DefaultDSNSecretKey]) != testPublicDSN || !ownedBy(secret, project) {
		t.Fatal("published Secret contract is invalid")
	}
	if err := kubeClient.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	_, phase, err = publisher.Reconcile(context.Background(), project, "payments", "", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseReady || authorizations != 2 {
		t.Fatalf("restore phase=%s auth=%d err=%v", phase, authorizations, err)
	}
}

func TestPublisherNeverOverwritesForeignSecret(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "checkout-sentry", Namespace: "payments"}, Data: map[string][]byte{"foreign": []byte("keep")}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	publisher := &Publisher{Client: kubeClient}
	called := false
	_, _, err := publisher.Reconcile(context.Background(), testProject(), "payments", "checkout-sentry", testPublicDSN, binding.DSNBinding{}, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrSecretConflict) || called {
		t.Fatalf("foreign conflict err=%v authorizeCalled=%v", err, called)
	}
	loaded := &corev1.Secret{}
	_ = kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: "checkout-sentry"}, loaded)
	if string(loaded.Data["foreign"]) != "keep" {
		t.Fatal("foreign Secret was modified")
	}
}

func TestPublisherTwoPhaseCutoverWaitsForExactConfirmation(t *testing.T) {
	t.Parallel()

	publisher, kubeClient := testPublisher(t)
	project := testProject()
	authorize := func(context.Context) error { return nil }
	state, _, err := publisher.Reconcile(context.Background(), project, "payments", "old-dsn", testPublicDSN, binding.DSNBinding{}, authorize)
	if err != nil {
		t.Fatal(err)
	}
	state, phase, err := publisher.Reconcile(context.Background(), project, "payments", "new-dsn", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseCutoverPending || state.CurrentSecret != "old-dsn" || state.PendingSecret != "new-dsn" {
		t.Fatalf("pending state=%#v phase=%s err=%v", state, phase, err)
	}
	for _, name := range []string{"old-dsn", "new-dsn"} {
		if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: name}, &corev1.Secret{}); err != nil {
			t.Fatalf("%s missing during cutover: %v", name, err)
		}
	}
	project.Annotations = map[string]string{ConfirmCutoverAnnotation: "wrong-name"}
	state, phase, err = publisher.Reconcile(context.Background(), project, "payments", "new-dsn", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseCutoverPending {
		t.Fatalf("wrong confirmation phase=%s err=%v", phase, err)
	}
	project.Annotations[ConfirmCutoverAnnotation] = "new-dsn"
	state, phase, err = publisher.Reconcile(context.Background(), project, "payments", "new-dsn", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseReady || state.CurrentSecret != "new-dsn" || state.PendingSecret != "" {
		t.Fatalf("confirmed state=%#v phase=%s err=%v", state, phase, err)
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: "old-dsn"}, &corev1.Secret{}); err == nil {
		t.Fatal("old Secret remains after confirmation")
	}
}

func TestPublisherReauthorizesEveryWriteAndPreservesWorkingSecretOnDenial(t *testing.T) {
	t.Parallel()

	publisher, kubeClient := testPublisher(t)
	project := testProject()
	state, _, err := publisher.Reconcile(context.Background(), project, "payments", "old-dsn", testPublicDSN, binding.DSNBinding{}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = publisher.Reconcile(context.Background(), project, "payments", "new-dsn", testPublicDSN, state, func(context.Context) error { return errors.New("policy changed") })
	if !errors.Is(err, ErrAuthorization) {
		t.Fatalf("authorization change error = %v", err)
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: "old-dsn"}, &corev1.Secret{}); err != nil {
		t.Fatalf("working Secret was removed: %v", err)
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: "new-dsn"}, &corev1.Secret{}); err == nil {
		t.Fatal("new Secret was written after authorization changed")
	}
}

func TestPublisherRevokesOnlyExactBoundSecrets(t *testing.T) {
	t.Parallel()

	publisher, kubeClient := testPublisher(t)
	project := testProject()
	authorize := func(context.Context) error { return nil }
	state, _, err := publisher.Reconcile(context.Background(), project, "payments", "old-dsn", testPublicDSN, binding.DSNBinding{}, authorize)
	if err != nil {
		t.Fatal(err)
	}
	state, phase, err := publisher.Reconcile(context.Background(), project, "payments", "new-dsn", testPublicDSN, state, authorize)
	if err != nil || phase != PhaseCutoverPending {
		t.Fatalf("pending state=%#v phase=%s err=%v", state, phase, err)
	}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: project.Namespace}, Data: map[string][]byte{"keep": []byte("true")}}
	if err := kubeClient.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	if err := publisher.Revoke(context.Background(), project, state, func(context.Context) error { authorizations++; return nil }); err != nil {
		t.Fatal(err)
	}
	if authorizations != 2 {
		t.Fatalf("authorization checks = %d, want 2", authorizations)
	}
	for _, name := range []string{"old-dsn", "new-dsn"} {
		err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: project.Namespace, Name: name}, &corev1.Secret{})
		if err == nil {
			t.Fatalf("bound Secret %q remains after revoke", name)
		}
	}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: project.Namespace, Name: "unrelated"}, &corev1.Secret{}); err != nil {
		t.Fatalf("unrelated Secret was affected: %v", err)
	}
}

func testPublisher(t *testing.T) (*Publisher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &Publisher{Client: kubeClient}, kubeClient
}

func testProject() *sentryv1alpha1.SentryProject {
	return &sentryv1alpha1.SentryProject{
		TypeMeta:   metav1.TypeMeta{APIVersion: sentryv1alpha1.GroupVersion.String(), Kind: "SentryProject"},
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("project-uid")},
	}
}
