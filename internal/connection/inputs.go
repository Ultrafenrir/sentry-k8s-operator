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

// Package connection loads and rotates connection-scoped secret inputs.
package connection

import (
	"context"
	"fmt"
	"net/url"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

// SecretInputs contains in-memory secret material. It must never be logged or written to status.
type SecretInputs struct {
	Token       []byte
	CABundle    []byte
	BotToken    []byte
	WebhookAuth []byte
}

// LoadSecretInputs reads all currently configured refs from the fixed operator namespace.
func LoadSecretInputs(ctx context.Context, reader client.Reader, namespace string, spec sentryv1alpha1.SentryConnectionSpec) (SecretInputs, error) {
	if namespace == "" {
		return SecretInputs{}, fmt.Errorf("operator namespace is required")
	}
	var inputs SecretInputs
	var err error
	if inputs.Token, err = loadSecretKey(ctx, reader, namespace, spec.Credentials.TokenSecretRef); err != nil {
		return SecretInputs{}, err
	}
	if spec.Credentials.CABundleSecretRef != nil {
		if inputs.CABundle, err = loadSecretKey(ctx, reader, namespace, *spec.Credentials.CABundleSecretRef); err != nil {
			return SecretInputs{}, err
		}
	}
	if spec.Integrations != nil && spec.Integrations.Telegram != nil {
		if err := validateReceiverURL(spec.Integrations.Telegram.ReceiverURL); err != nil {
			return SecretInputs{}, err
		}
		if inputs.BotToken, err = loadSecretKey(ctx, reader, namespace, spec.Integrations.Telegram.BotTokenSecretRef); err != nil {
			return SecretInputs{}, err
		}
		if inputs.WebhookAuth, err = loadSecretKey(ctx, reader, namespace, spec.Integrations.Telegram.WebhookAuthSecretRef); err != nil {
			return SecretInputs{}, err
		}
	}
	return inputs, nil
}

// SecretNames returns the unique configured Secret names used by one connection.
func SecretNames(spec sentryv1alpha1.SentryConnectionSpec) []string {
	seen := map[string]struct{}{}
	add := func(ref sentryv1alpha1.SecretKeyReference) { seen[ref.Name] = struct{}{} }
	add(spec.Credentials.TokenSecretRef)
	if spec.Credentials.CABundleSecretRef != nil {
		add(*spec.Credentials.CABundleSecretRef)
	}
	if spec.Integrations != nil && spec.Integrations.Telegram != nil {
		add(spec.Integrations.Telegram.BotTokenSecretRef)
		add(spec.Integrations.Telegram.WebhookAuthSecretRef)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func loadSecretKey(ctx context.Context, reader client.Reader, namespace string, ref sentryv1alpha1.SecretKeyReference) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("read referenced Secret %q: %w", ref.Name, err)
	}
	value, found := secret.Data[ref.Key]
	if !found || len(value) == 0 {
		return nil, fmt.Errorf("referenced Secret %q has no non-empty key %q", ref.Name, ref.Key)
	}
	return append([]byte(nil), value...), nil
}

func validateReceiverURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "/events" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("Telegram Sentry-hook receiver URL must be exact absolute HTTPS /events base")
	}
	return nil
}
