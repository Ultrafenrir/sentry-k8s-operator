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

package connection

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestLoadSecretInputsAndNames(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "operator"}, Data: map[string][]byte{"token": []byte("api-secret"), "ca": []byte("ca-pem")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "telegram", Namespace: "operator"}, Data: map[string][]byte{"bot": []byte("bot-secret"), "webhook": []byte("webhook-secret")}},
	).Build()
	caRef := &sentryv1alpha1.SecretKeyReference{Name: "api", Key: "ca"}
	spec := sentryv1alpha1.SentryConnectionSpec{
		Credentials: sentryv1alpha1.ConnectionCredentials{
			TokenSecretRef:    sentryv1alpha1.SecretKeyReference{Name: "api", Key: "token"},
			CABundleSecretRef: caRef,
		},
		Integrations: &sentryv1alpha1.ConnectionIntegrations{Telegram: &sentryv1alpha1.TelegramIntegration{
			BotTokenSecretRef:    sentryv1alpha1.SecretKeyReference{Name: "telegram", Key: "bot"},
			ReceiverURL:          "https://receiver.example/events",
			WebhookAuthSecretRef: sentryv1alpha1.SecretKeyReference{Name: "telegram", Key: "webhook"},
		}},
	}
	inputs, err := LoadSecretInputs(context.Background(), reader, "operator", spec)
	if err != nil {
		t.Fatalf("LoadSecretInputs: %v", err)
	}
	if string(inputs.Token) != "api-secret" || string(inputs.CABundle) != "ca-pem" || string(inputs.BotToken) != "bot-secret" || string(inputs.WebhookAuth) != "webhook-secret" {
		t.Fatal("one or more secret values were not loaded")
	}
	names := SecretNames(spec)
	if len(names) != 2 {
		t.Fatalf("deduplicated names = %v", names)
	}
	inputs.Token[0] = 'X'
	var secret corev1.Secret
	if err := reader.Get(context.Background(), clientKey("operator", "api"), &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["token"]) != "api-secret" {
		t.Fatal("loaded value aliased the Secret cache")
	}
}

func TestLoadSecretInputsRejectsUnsafeReceiverAndEmptyKey(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "operator"}, Data: map[string][]byte{"token": []byte("token")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "telegram", Namespace: "operator"}, Data: map[string][]byte{"bot": []byte("bot"), "webhook": []byte("webhook")}},
	).Build()
	spec := sentryv1alpha1.SentryConnectionSpec{
		Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{Name: "api", Key: "token"}},
		Integrations: &sentryv1alpha1.ConnectionIntegrations{Telegram: &sentryv1alpha1.TelegramIntegration{
			BotTokenSecretRef:    sentryv1alpha1.SecretKeyReference{Name: "telegram", Key: "bot"},
			ReceiverURL:          "https://user:password@receiver.example/hook",
			WebhookAuthSecretRef: sentryv1alpha1.SecretKeyReference{Name: "telegram", Key: "webhook"},
		}},
	}
	if _, err := LoadSecretInputs(context.Background(), reader, "operator", spec); err == nil {
		t.Fatal("receiver URL with userinfo was accepted")
	}
	for _, invalid := range []string{"https://receiver.example/events/", "https://receiver.example/events?source=private", "https://receiver.example/other"} {
		spec.Integrations.Telegram.ReceiverURL = invalid
		if _, err := LoadSecretInputs(context.Background(), reader, "operator", spec); err == nil {
			t.Fatalf("unsafe receiver base accepted: %q", invalid)
		}
	}
	spec.Integrations = nil
	spec.Credentials.TokenSecretRef.Key = "missing"
	if _, err := LoadSecretInputs(context.Background(), reader, "operator", spec); err == nil {
		t.Fatal("missing Secret key was accepted")
	}
}

func clientKey(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}
