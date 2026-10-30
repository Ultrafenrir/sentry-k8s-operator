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

// Package statebackup implements a create-only, integrity-checked backup core
// for protected operator records. Bundles contain credentials, identities and
// message payloads and therefore must be encrypted by the platform backup
// system before leaving trusted memory or storage.
package statebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

const (
	CurrentVersion   = "sentryconfiguration.io/protected-state-backup/v1alpha1"
	recordTypeLabel  = "sentryconfiguration.io/record-type"
	recordStateLabel = "sentryconfiguration.io/delivery-state"
	managedByLabel   = "app.kubernetes.io/managed-by"
)

var (
	ErrInvalid  = errors.New("protected state backup is invalid")
	ErrConflict = errors.New("protected state restore conflicts with existing state")
)

type Namespaces struct {
	State       string `json:"state"`
	Routing     string `json:"routing"`
	Delivery    string `json:"delivery"`
	HookSources string `json:"hookSources"`
}

type Bundle struct {
	Version    string     `json:"version"`
	CreatedAt  time.Time  `json:"createdAt"`
	Namespaces Namespaces `json:"namespaces"`
	Records    []Record   `json:"records"`
	Digest     string     `json:"digest"`
}

// Record deliberately excludes UID, resourceVersion, managedFields and
// timestamps. Restored objects receive fresh Kubernetes CAS identities.
type Record struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	RecordType string            `json:"recordType"`
	Immutable  bool              `json:"immutable"`
	Labels     map[string]string `json:"labels"`
	Data       map[string][]byte `json:"data"`
	Digest     string            `json:"digest"`
}

type Store struct {
	Client     client.Client
	Namespaces Namespaces
	Now        func() time.Time
}

type RestoreResult struct {
	Created  int
	Existing int
}

func (s *Store) Export(ctx context.Context) (Bundle, error) {
	if err := s.validate(); err != nil {
		return Bundle{}, err
	}
	bundle := Bundle{Version: CurrentVersion, CreatedAt: s.now(), Namespaces: s.Namespaces}
	for _, namespace := range protectedNamespaces(s.Namespaces) {
		secrets := &corev1.SecretList{}
		if err := s.Client.List(ctx, secrets, client.InNamespace(namespace)); err != nil {
			return Bundle{}, err
		}
		for index := range secrets.Items {
			record, err := recordFromSecret(&secrets.Items[index], s.Namespaces)
			if err != nil {
				return Bundle{}, err
			}
			bundle.Records = append(bundle.Records, record)
		}
	}
	sort.Slice(bundle.Records, func(i, j int) bool {
		if bundle.Records[i].Namespace == bundle.Records[j].Namespace {
			return bundle.Records[i].Name < bundle.Records[j].Name
		}
		return bundle.Records[i].Namespace < bundle.Records[j].Namespace
	})
	digest, err := bundleDigest(bundle)
	if err != nil {
		return Bundle{}, err
	}
	bundle.Digest = digest
	return bundle, nil
}

// Restore is create-only and resumable. Existing records are accepted only if
// every protected byte and metadata field matches the bundle. It never grants
// ownership, changes CR status or calls an external API.
func (s *Store) Restore(ctx context.Context, bundle Bundle) (RestoreResult, error) {
	if err := s.validate(); err != nil {
		return RestoreResult{}, err
	}
	if err := validateBundle(bundle, s.Namespaces); err != nil {
		return RestoreResult{}, err
	}
	expected := make(map[types.NamespacedName]Record, len(bundle.Records))
	for _, record := range bundle.Records {
		expected[types.NamespacedName{Namespace: record.Namespace, Name: record.Name}] = record
	}

	// Preflight every existing Secret before creating anything. A resume may
	// contain an identical subset; any extra or changed object fails closed.
	result := RestoreResult{}
	for _, namespace := range protectedNamespaces(s.Namespaces) {
		secrets := &corev1.SecretList{}
		if err := s.Client.List(ctx, secrets, client.InNamespace(namespace)); err != nil {
			return RestoreResult{}, err
		}
		for index := range secrets.Items {
			key := types.NamespacedName{Namespace: namespace, Name: secrets.Items[index].Name}
			wanted, ok := expected[key]
			if !ok || !secretMatchesRecord(&secrets.Items[index], wanted, s.Namespaces) {
				return RestoreResult{}, ErrConflict
			}
			result.Existing++
		}
	}

	for _, record := range bundle.Records {
		key := types.NamespacedName{Namespace: record.Namespace, Name: record.Name}
		current := &corev1.Secret{}
		err := s.Client.Get(ctx, key, current)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return result, err
		}
		secret := secretFromRecord(record)
		if err := s.Client.Create(ctx, secret); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return result, err
			}
			if getErr := s.Client.Get(ctx, key, current); getErr != nil || !secretMatchesRecord(current, record, s.Namespaces) {
				return result, ErrConflict
			}
			result.Existing++
			continue
		}
		result.Created++
	}
	return result, nil
}

func validateBundle(bundle Bundle, namespaces Namespaces) error {
	if bundle.Version != CurrentVersion || bundle.CreatedAt.IsZero() || bundle.Namespaces != namespaces || !validDigest(bundle.Digest) {
		return ErrInvalid
	}
	seen := map[types.NamespacedName]struct{}{}
	previous := types.NamespacedName{}
	for index, record := range bundle.Records {
		if err := validateRecord(record, namespaces); err != nil {
			return err
		}
		key := types.NamespacedName{Namespace: record.Namespace, Name: record.Name}
		if _, ok := seen[key]; ok {
			return ErrInvalid
		}
		if index > 0 && (key.Namespace < previous.Namespace || (key.Namespace == previous.Namespace && key.Name < previous.Name)) {
			return ErrInvalid
		}
		seen[key] = struct{}{}
		previous = key
	}
	wanted, err := bundleDigest(bundle)
	if err != nil || wanted != bundle.Digest {
		return ErrInvalid
	}
	return nil
}

func recordFromSecret(secret *corev1.Secret, namespaces Namespaces) (Record, error) {
	if secret == nil || secret.Namespace == "" || secret.Name == "" || secret.Type != corev1.SecretTypeOpaque || len(secret.Annotations) != 0 || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 {
		return Record{}, ErrInvalid
	}
	record := Record{
		Namespace: secret.Namespace, Name: secret.Name, RecordType: secret.Labels[recordTypeLabel],
		Immutable: secret.Immutable != nil && *secret.Immutable,
		Labels:    maps.Clone(secret.Labels), Data: cloneData(secret.Data),
	}
	digest, err := recordDigest(record)
	if err != nil {
		return Record{}, err
	}
	record.Digest = digest
	if err := validateRecord(record, namespaces); err != nil {
		return Record{}, err
	}
	return record, nil
}

func validateRecord(record Record, namespaces Namespaces) error {
	if record.Namespace == "" || record.Name == "" || record.RecordType == "" || record.Labels[recordTypeLabel] != record.RecordType || !validDigest(record.Digest) {
		return ErrInvalid
	}
	for key := range record.Labels {
		if key != recordTypeLabel && key != recordStateLabel && key != managedByLabel {
			return ErrInvalid
		}
	}
	dataKey, immutable, ok := recordContract(record.Namespace, record.RecordType, namespaces)
	if !ok || record.Immutable != immutable || len(record.Data) != 1 || len(record.Data[dataKey]) == 0 {
		return ErrInvalid
	}
	if record.RecordType == "telegram-delivery" && record.Labels[recordStateLabel] == "" {
		return ErrInvalid
	}
	if record.RecordType == "telegram-delivery-admission" && record.Labels[recordStateLabel] != "" {
		return ErrInvalid
	}
	if record.RecordType == "sentry-hook-source" && record.Labels[managedByLabel] != "sentry-kubernetes-operator" {
		return ErrInvalid
	}
	if err := validatePayload(record, dataKey, namespaces); err != nil {
		return err
	}
	wanted, err := recordDigest(record)
	if err != nil || wanted != record.Digest {
		return ErrInvalid
	}
	return nil
}

func validatePayload(record Record, dataKey string, namespaces Namespaces) error {
	switch {
	case record.Namespace == namespaces.State && record.RecordType == "project-binding":
		var value binding.ProjectBinding
		if json.Unmarshal(record.Data[dataKey], &value) != nil || binding.ValidateProjectBinding(value) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.State && record.RecordType == "manager-claim":
		var value binding.ManagerClaim
		if json.Unmarshal(record.Data[dataKey], &value) != nil || binding.ValidateManagerClaim(value) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.State && record.RecordType == "operation-journal":
		var value sentryapi.Operation
		if json.Unmarshal(record.Data[dataKey], &value) != nil || binding.ValidateOperation(value) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.State && record.RecordType == "telegram-challenge-index":
		if registration.ValidateChallengeIndexPayload(record.Name, record.Data[dataKey]) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.HookSources && record.RecordType == "sentry-hook-source":
		if sentryhook.ValidateSourceRecordPayload(record.Name, record.Data[dataKey]) != nil {
			return ErrInvalid
		}
	case (record.Namespace == namespaces.State || record.Namespace == namespaces.Routing) && record.RecordType == "telegram-route":
		if registration.ValidateRoutePayload(record.Name, record.Data[dataKey]) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.Delivery && record.RecordType == "telegram-delivery":
		if delivery.ValidateRecordPayload(record.Name, record.Labels[recordStateLabel], record.Data[dataKey]) != nil {
			return ErrInvalid
		}
	case record.Namespace == namespaces.Delivery && record.RecordType == "telegram-delivery-admission":
		if delivery.ValidateAdmissionPayload(record.Name, record.Data[dataKey]) != nil {
			return ErrInvalid
		}
	}
	return nil
}

func recordContract(namespace, recordType string, namespaces Namespaces) (string, bool, bool) {
	switch namespace {
	case namespaces.State:
		switch recordType {
		case "project-binding", "manager-claim", "operation-journal":
			return "state.json", false, true
		case "telegram-challenge-index":
			return "challenge.json", true, true
		case "telegram-route": // supported only for resumable legacy migration
			return "route.json", true, true
		}
	case namespaces.Routing:
		if recordType == "telegram-route" {
			return "route.json", true, true
		}
	case namespaces.Delivery:
		switch recordType {
		case "telegram-delivery":
			return "delivery.json", false, true
		case "telegram-delivery-admission":
			return "admission.json", true, true
		}
	case namespaces.HookSources:
		if recordType == "sentry-hook-source" {
			return "source.json", false, true
		}
	}
	return "", false, false
}

func secretFromRecord(record Record) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: record.Namespace, Name: record.Name, Labels: maps.Clone(record.Labels)},
		Type:       corev1.SecretTypeOpaque, Data: cloneData(record.Data),
	}
	if record.Immutable {
		immutable := true
		secret.Immutable = &immutable
	}
	return secret
}

func secretMatchesRecord(secret *corev1.Secret, record Record, namespaces Namespaces) bool {
	current, err := recordFromSecret(secret, namespaces)
	return err == nil && current.Digest == record.Digest
}

func recordDigest(record Record) (string, error) {
	value := struct {
		Namespace  string            `json:"namespace"`
		Name       string            `json:"name"`
		RecordType string            `json:"recordType"`
		Immutable  bool              `json:"immutable"`
		Labels     map[string]string `json:"labels"`
		Data       map[string][]byte `json:"data"`
	}{record.Namespace, record.Name, record.RecordType, record.Immutable, record.Labels, record.Data}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func bundleDigest(bundle Bundle) (string, error) {
	value := struct {
		Version    string     `json:"version"`
		CreatedAt  time.Time  `json:"createdAt"`
		Namespaces Namespaces `json:"namespaces"`
		Records    []Record   `json:"records"`
	}{bundle.Version, bundle.CreatedAt, bundle.Namespaces, bundle.Records}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func cloneData(source map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(source))
	for key, value := range source {
		result[key] = slices.Clone(value)
	}
	return result
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}

func (s *Store) validate() error {
	if s == nil || s.Client == nil || !distinctNonEmpty(protectedNamespaces(s.Namespaces)) {
		return ErrInvalid
	}
	return nil
}

func protectedNamespaces(namespaces Namespaces) []string {
	return []string{namespaces.State, namespaces.Routing, namespaces.Delivery, namespaces.HookSources}
}

func distinctNonEmpty(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, found := seen[value]; found {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Round(0)
	}
	return time.Now().UTC().Round(0)
}
