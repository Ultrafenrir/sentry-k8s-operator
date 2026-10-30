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
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DefaultVerifierSecretName = "sentry-hook-receiver-sources"

var ErrVerifierConfig = errors.New("Sentry hook verifier configuration is invalid")

type VerifierConfig struct {
	ExpectedProjectSlug string   `json:"expectedProjectSlug"`
	HMACSecretsBase64   []string `json:"hmacSecretsBase64"`
}

// VerifierStore updates one source entry in an administrator-created aggregate Secret.
// It never creates the Secret and preserves every sibling entry. Kubernetes
// ResourceVersion supplies the HA compare-and-swap boundary.
type VerifierStore struct {
	Client     client.Client
	Namespace  string
	SecretName string
}

func (s *VerifierStore) Get(ctx context.Context, sourceBindingKey string) ([]byte, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || !canonicalHash(sourceBindingKey) {
		return nil, ErrVerifierConfig
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.name()}, secret); err != nil {
		return nil, err
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return nil, ErrVerifierConfig
	}
	payload, found := secret.Data[verifierDataKey(sourceBindingKey)]
	if !found {
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), verifierDataKey(sourceBindingKey))
	}
	if ValidateVerifierConfig(payload) != nil {
		return nil, ErrVerifierConfig
	}
	return append([]byte(nil), payload...), nil
}

func (s *VerifierStore) Upsert(ctx context.Context, sourceBindingKey string, payload []byte) error {
	if s == nil || s.Client == nil || s.Namespace == "" || !canonicalHash(sourceBindingKey) || ValidateVerifierConfig(payload) != nil {
		return ErrVerifierConfig
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.name()}, secret); err != nil {
		return err
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return ErrVerifierConfig
	}
	key := verifierDataKey(sourceBindingKey)
	if bytes.Equal(secret.Data[key], payload) {
		return nil
	}
	next := secret.DeepCopy()
	if next.Data == nil {
		next.Data = map[string][]byte{}
	}
	next.Data[key] = append([]byte(nil), payload...)
	return s.Client.Update(ctx, next)
}

// CompareAndSwap replaces one verifier entry only when its exact validated payload still
// matches previousPayload. The aggregate Secret resourceVersion protects the subsequent
// Kubernetes update, while the byte comparison prevents a stale rotation plan from
// overwriting a newer rotation of the same source.
func (s *VerifierStore) CompareAndSwap(ctx context.Context, sourceBindingKey string, previousPayload, nextPayload []byte) error {
	if s == nil || s.Client == nil || s.Namespace == "" || !canonicalHash(sourceBindingKey) ||
		ValidateVerifierConfig(previousPayload) != nil || ValidateVerifierConfig(nextPayload) != nil {
		return ErrVerifierConfig
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.name()}, secret); err != nil {
		return err
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return ErrVerifierConfig
	}
	key := verifierDataKey(sourceBindingKey)
	current, found := secret.Data[key]
	if !found || !bytes.Equal(current, previousPayload) {
		return ErrVerifierConfig
	}
	if bytes.Equal(current, nextPayload) {
		return nil
	}
	next := secret.DeepCopy()
	next.Data[key] = append([]byte(nil), nextPayload...)
	return s.Client.Update(ctx, next)
}

func (s *VerifierStore) DeleteExact(ctx context.Context, sourceBindingKey string, expectedPayload []byte) error {
	if s == nil || s.Client == nil || s.Namespace == "" || !canonicalHash(sourceBindingKey) || ValidateVerifierConfig(expectedPayload) != nil {
		return ErrVerifierConfig
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.name()}, secret); err != nil {
		return err
	}
	key := verifierDataKey(sourceBindingKey)
	current, found := secret.Data[key]
	if !found {
		return nil
	}
	if !bytes.Equal(current, expectedPayload) {
		return ErrVerifierConfig
	}
	next := secret.DeepCopy()
	delete(next.Data, key)
	return s.Client.Update(ctx, next)
}

func EncodeVerifierConfig(expectedProjectSlug string, secrets [][]byte) ([]byte, error) {
	configuration := VerifierConfig{ExpectedProjectSlug: expectedProjectSlug, HMACSecretsBase64: make([]string, len(secrets))}
	for index, secret := range secrets {
		configuration.HMACSecretsBase64[index] = base64.StdEncoding.EncodeToString(secret)
	}
	payload, err := json.Marshal(configuration)
	if err != nil || ValidateVerifierConfig(payload) != nil {
		return nil, ErrVerifierConfig
	}
	return payload, nil
}

// PlanVerifierRotation returns the config used before switching the external hook and the
// config used after that switch is independently verified. An existing two-key overlap may
// only be completed or rolled back; a third concurrent key fails closed.
func PlanVerifierRotation(currentPayload, desiredSecret []byte) (overlap, retired []byte, err error) {
	configuration, currentSecrets, err := decodeVerifierConfig(currentPayload)
	if err != nil || len(desiredSecret) < 16 || len(desiredSecret) > 4096 {
		return nil, nil, ErrVerifierConfig
	}
	desired := append([]byte(nil), desiredSecret...)
	switch len(currentSecrets) {
	case 1:
		if slices.Equal(currentSecrets[0], desired) {
			overlap = append([]byte(nil), currentPayload...)
		} else {
			overlap, err = EncodeVerifierConfig(configuration.ExpectedProjectSlug, [][]byte{currentSecrets[0], desired})
		}
	case 2:
		if !slices.Equal(currentSecrets[0], desired) && !slices.Equal(currentSecrets[1], desired) {
			return nil, nil, ErrVerifierConfig
		}
		overlap = append([]byte(nil), currentPayload...)
	default:
		return nil, nil, ErrVerifierConfig
	}
	if err != nil {
		return nil, nil, err
	}
	retired, err = EncodeVerifierConfig(configuration.ExpectedProjectSlug, [][]byte{desired})
	if err != nil {
		return nil, nil, err
	}
	return overlap, retired, nil
}

func ValidateVerifierConfig(payload []byte) error {
	_, _, err := decodeVerifierConfig(payload)
	return err
}

func decodeVerifierConfig(payload []byte) (VerifierConfig, [][]byte, error) {
	if len(payload) == 0 || len(payload) > sourceConfigLimit {
		return VerifierConfig{}, nil, ErrVerifierConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var configuration VerifierConfig
	if decoder.Decode(&configuration) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validSlug(configuration.ExpectedProjectSlug) ||
		len(configuration.HMACSecretsBase64) == 0 || len(configuration.HMACSecretsBase64) > 2 {
		return VerifierConfig{}, nil, ErrVerifierConfig
	}
	secrets := make([][]byte, len(configuration.HMACSecretsBase64))
	seen := map[string]struct{}{}
	for index, encoded := range configuration.HMACSecretsBase64 {
		secret, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(secret) < 16 || len(secret) > 4096 || base64.StdEncoding.EncodeToString(secret) != encoded {
			return VerifierConfig{}, nil, ErrVerifierConfig
		}
		if _, duplicate := seen[string(secret)]; duplicate {
			return VerifierConfig{}, nil, ErrVerifierConfig
		}
		seen[string(secret)] = struct{}{}
		secrets[index] = secret
	}
	return configuration, secrets, nil
}

func (s *VerifierStore) name() string {
	if s.SecretName != "" {
		return s.SecretName
	}
	return DefaultVerifierSecretName
}

func verifierDataKey(sourceBindingKey string) string { return sourceBindingKey + ".json" }
