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
	"encoding/json"
	"errors"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	sourceRecordVersion = "v1alpha1"
	sourceDataKey       = "source.json"
	sourceRecordType    = "sentry-hook-source"
	sourceRecordPrefix  = "sentry-hook-source-"
	sourceRecordLabel   = "sentryconfiguration.io/record-type"
)

var ErrSourceRecord = errors.New("Sentry hook source record is invalid")

// SourceRecord is the protected local authorization boundary between an authenticated
// Sentry hook and Telegram routes. Raw event fields and external Sentry identities are
// deliberately not representable. ResourceVersion is only a Kubernetes CAS token.
type SourceRecord struct {
	Version             string             `json:"version"`
	Revision            int64              `json:"revision"`
	SourceBindingKey    string             `json:"sourceBindingKey"`
	ExpectedProjectSlug string             `json:"expectedProjectSlug"`
	Deliveries          []TelegramDelivery `json:"deliveries"`
	ResourceVersion     string             `json:"-"`
}

type SourceStore struct {
	Client    client.Client
	Namespace string
}

func (s *SourceStore) Create(ctx context.Context, record SourceRecord) (SourceRecord, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || record.Revision != 1 || validateSourceRecord(record) != nil {
		return SourceRecord{}, ErrSourceRecord
	}
	secret, err := encodeSourceSecret(s.Namespace, record)
	if err != nil {
		return SourceRecord{}, err
	}
	if err := s.Client.Create(ctx, secret); err != nil {
		return SourceRecord{}, err
	}
	record.ResourceVersion = secret.ResourceVersion
	return record, nil
}

func (s *SourceStore) Get(ctx context.Context, sourceBindingKey string) (SourceRecord, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || !canonicalHash(sourceBindingKey) {
		return SourceRecord{}, ErrSourceRecord
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: sourceRecordName(sourceBindingKey)}, secret); err != nil {
		return SourceRecord{}, err
	}
	return decodeSourceSecret(secret, sourceBindingKey)
}

// CompareAndSwap changes only the local delivery plan. Source identity and expected Sentry
// project slug are stable for the lifetime of a record; changing either requires a new key.
func (s *SourceStore) CompareAndSwap(ctx context.Context, previous, next SourceRecord) (SourceRecord, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || previous.ResourceVersion == "" ||
		next.Revision != previous.Revision+1 || next.SourceBindingKey != previous.SourceBindingKey ||
		next.ExpectedProjectSlug != previous.ExpectedProjectSlug || validateSourceRecord(next) != nil {
		return SourceRecord{}, ErrSourceRecord
	}
	secret, err := encodeSourceSecret(s.Namespace, next)
	if err != nil {
		return SourceRecord{}, err
	}
	secret.ResourceVersion = previous.ResourceVersion
	if err := s.Client.Update(ctx, secret); err != nil {
		return SourceRecord{}, err
	}
	next.ResourceVersion = secret.ResourceVersion
	return next, nil
}

func (s *SourceStore) DeleteExact(ctx context.Context, record SourceRecord) error {
	if s == nil || s.Client == nil || s.Namespace == "" || record.ResourceVersion == "" || validateSourceRecord(record) != nil {
		return ErrSourceRecord
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: sourceRecordName(record.SourceBindingKey)}}
	resourceVersion := record.ResourceVersion
	return s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion})
}

func (s *SourceStore) ResolveTelegramDeliveries(ctx context.Context, sourceBindingKey string) ([]TelegramDelivery, error) {
	record, err := s.Get(ctx, sourceBindingKey)
	if err != nil {
		return nil, ErrSourceRecord
	}
	result := make([]TelegramDelivery, len(record.Deliveries))
	copy(result, record.Deliveries)
	return result, nil
}

func encodeSourceSecret(namespace string, record SourceRecord) (*corev1.Secret, error) {
	if namespace == "" || validateSourceRecord(record) != nil {
		return nil, ErrSourceRecord
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, ErrSourceRecord
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: sourceRecordName(record.SourceBindingKey),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sentry-kubernetes-operator",
				sourceRecordLabel:              sourceRecordType,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{sourceDataKey: payload},
	}, nil
}

func decodeSourceSecret(secret *corev1.Secret, expectedKey string) (SourceRecord, error) {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Labels[sourceRecordLabel] != sourceRecordType ||
		secret.Labels["app.kubernetes.io/managed-by"] != "sentry-kubernetes-operator" ||
		len(secret.Data) != 1 || len(secret.Data[sourceDataKey]) == 0 {
		return SourceRecord{}, ErrSourceRecord
	}
	var record SourceRecord
	if json.Unmarshal(secret.Data[sourceDataKey], &record) != nil || validateSourceRecord(record) != nil ||
		secret.Name != sourceRecordName(record.SourceBindingKey) || (expectedKey != "" && record.SourceBindingKey != expectedKey) {
		return SourceRecord{}, ErrSourceRecord
	}
	record.ResourceVersion = secret.ResourceVersion
	return record, nil
}

func ValidateSourceRecordPayload(name string, payload []byte) error {
	var record SourceRecord
	if json.Unmarshal(payload, &record) != nil || validateSourceRecord(record) != nil || name != sourceRecordName(record.SourceBindingKey) {
		return ErrSourceRecord
	}
	return nil
}

func validateSourceRecord(record SourceRecord) error {
	if record.Version != sourceRecordVersion || record.Revision <= 0 || !canonicalHash(record.SourceBindingKey) ||
		!validSlug(record.ExpectedProjectSlug) || len(record.Deliveries) > 100 {
		return ErrSourceRecord
	}
	previousRoute := ""
	for _, candidate := range record.Deliveries {
		if !canonicalHash(candidate.RouteKey) || candidate.RouteKey <= previousRoute {
			return ErrSourceRecord
		}
		if _, err := Render(candidate.Render); err != nil {
			return ErrSourceRecord
		}
		previousRoute = candidate.RouteKey
	}
	return nil
}

func sourceRecordName(sourceBindingKey string) string {
	return sourceRecordPrefix + sourceBindingKey
}

func SortTelegramDeliveries(deliveries []TelegramDelivery) []TelegramDelivery {
	result := append([]TelegramDelivery(nil), deliveries...)
	sort.Slice(result, func(i, j int) bool { return result[i].RouteKey < result[j].RouteKey })
	return result
}

var _ DeliveryResolver = (*SourceStore)(nil)
