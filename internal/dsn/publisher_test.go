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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

const testPublicDSN = "https://public@sentry.example/1"

func TestPublisherPatchesOnlyProjectKeyInPrecreatedSecret(t *testing.T) {
	t.Parallel()
	publisher, kubeClient := testPublisher(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sentryv1alpha1.DSNSecretName, Namespace: "payments", Labels: map[string]string{"team": "payments"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"another-project": []byte("keep")},
	})
	project := testProject()
	authorizations := 0
	state, phase, err := publisher.Reconcile(context.Background(), project, testPublicDSN, binding.DSNBinding{}, func(context.Context) error {
		authorizations++
		return nil
	})
	if err != nil || phase != PhaseReady || state.CurrentSecret != sentryv1alpha1.DSNSecretName || state.PendingSecret != "" || authorizations != 1 {
		t.Fatalf("state=%#v phase=%s auth=%d err=%v", state, phase, authorizations, err)
	}
	secret := loadDSNSecret(t, kubeClient)
	if string(secret.Data[project.Name]) != testPublicDSN || string(secret.Data["another-project"]) != "keep" || secret.Labels["team"] != "payments" {
		t.Fatalf("shared Secret was not patched safely: %#v", secret)
	}
	_, _, err = publisher.Reconcile(context.Background(), project, testPublicDSN, state, func(context.Context) error {
		authorizations++
		return nil
	})
	if err != nil || authorizations != 1 {
		t.Fatalf("idempotent reconcile authorized a write: auth=%d err=%v", authorizations, err)
	}
}

func TestPublisherRequiresPrecreatedFixedNameSecret(t *testing.T) {
	t.Parallel()
	publisher, kubeClient := testPublisher(t)
	called := false
	_, _, err := publisher.Reconcile(context.Background(), testProject(), testPublicDSN, binding.DSNBinding{}, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrSecretMissing) || called {
		t.Fatalf("missing Secret err=%v authorizeCalled=%v", err, called)
	}
	secrets := &corev1.SecretList{}
	if err := kubeClient.List(context.Background(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("publisher created a Secret: items=%d err=%v", len(secrets.Items), err)
	}
}

func TestPublisherPreservesWorkingValueWhenAuthorizationChanges(t *testing.T) {
	t.Parallel()
	publisher, kubeClient := testPublisher(t, emptyDSNSecret())
	project := testProject()
	state, _, err := publisher.Reconcile(context.Background(), project, testPublicDSN, binding.DSNBinding{}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = publisher.Reconcile(context.Background(), project, "https://replacement@sentry.example/1", state, func(context.Context) error {
		return errors.New("policy changed")
	})
	if !errors.Is(err, ErrAuthorization) {
		t.Fatalf("authorization change error = %v", err)
	}
	if got := string(loadDSNSecret(t, kubeClient).Data[project.Name]); got != testPublicDSN {
		t.Fatalf("working DSN changed to %q", got)
	}
}

func TestPublisherRevokesOnlyProjectKey(t *testing.T) {
	t.Parallel()
	publisher, kubeClient := testPublisher(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sentryv1alpha1.DSNSecretName, Namespace: "payments"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"checkout":        []byte(testPublicDSN),
			"another-project": []byte("keep"),
		},
	})
	state := binding.DSNBinding{CurrentSecret: sentryv1alpha1.DSNSecretName}
	authorizations := 0
	if err := publisher.Revoke(context.Background(), testProject(), state, func(context.Context) error {
		authorizations++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secret := loadDSNSecret(t, kubeClient)
	if _, found := secret.Data["checkout"]; found || string(secret.Data["another-project"]) != "keep" || authorizations != 1 {
		t.Fatalf("shared Secret revoke was unsafe: data=%#v auth=%d", secret.Data, authorizations)
	}
	if err := publisher.Revoke(context.Background(), testProject(), state, func(context.Context) error {
		authorizations++
		return nil
	}); err != nil || authorizations != 1 {
		t.Fatalf("idempotent revoke authorized a write: auth=%d err=%v", authorizations, err)
	}
}

func TestPublisherDoesNotRecreateMissingSharedSecretDuringRevoke(t *testing.T) {
	t.Parallel()
	publisher, kubeClient := testPublisher(t, emptyDSNSecret())
	if err := kubeClient.Delete(context.Background(), loadDSNSecret(t, kubeClient)); err != nil {
		t.Fatal(err)
	}
	state := binding.DSNBinding{CurrentSecret: sentryv1alpha1.DSNSecretName}
	if err := publisher.Revoke(context.Background(), testProject(), state, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: sentryv1alpha1.DSNSecretName}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("shared Secret unexpectedly recreated: %v", err)
	}
}

func testPublisher(t *testing.T, objects ...client.Object) (*Publisher, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &Publisher{Client: kubeClient}, kubeClient
}

func emptyDSNSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sentryv1alpha1.DSNSecretName, Namespace: "payments"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
}

func loadDSNSecret(t *testing.T, kubeClient client.Client) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: "payments", Name: sentryv1alpha1.DSNSecretName}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func testProject() *sentryv1alpha1.SentryProject {
	return &sentryv1alpha1.SentryProject{
		TypeMeta:   metav1.TypeMeta{APIVersion: sentryv1alpha1.GroupVersion.String(), Kind: "SentryProject"},
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("project-uid")},
	}
}
