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

package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	RecordTypeLabel = "sentryconfiguration.io/record-type"
	RecordTypeAudit = "mutation-audit"
	EventDataKey    = "event.json"
	auditNamePrefix = "sentry-audit-"
)

var ErrStore = errors.New("durable mutation audit store failed")

// KubernetesStore is an append-only audit Sink. Each event is one immutable Secret so a
// successful Record is a durable serialization point before an external mutation.
// Retention is deliberately separate and leader-only.
type KubernetesStore struct {
	Client    client.Client
	Namespace string
	Logger    logr.Logger
	Now       func() time.Time
	Random    io.Reader
}

type StoredEvent struct {
	Name            string `json:"-"`
	ResourceVersion string `json:"-"`
	Event
}

func (s *KubernetesStore) Record(ctx context.Context, event Event) error {
	if s == nil || s.Client == nil || s.Namespace == "" {
		return ErrStore
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = s.now()
	}
	if err := Validate(event); err != nil {
		return ErrStore
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return ErrStore
	}
	random := make([]byte, 8)
	reader := s.Random
	if reader == nil {
		reader = rand.Reader
	}
	if n, err := io.ReadFull(reader, random); err != nil || n != len(random) {
		return ErrStore
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Namespace,
			Name:      auditNamePrefix + strconv.FormatInt(event.Timestamp.UnixNano(), 36) + "-" + hex.EncodeToString(random),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sentry-kubernetes-operator",
				RecordTypeLabel:                RecordTypeAudit,
			},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{EventDataKey: payload},
	}
	if err := s.Client.Create(ctx, secret); err != nil {
		return ErrStore
	}
	if s.Logger.GetSink() != nil {
		logEvent(s.Logger, event)
	}
	return nil
}

// List returns validated audit events in deterministic timestamp/name order. One malformed
// record fails the entire export so corruption cannot be silently omitted.
func (s *KubernetesStore) List(ctx context.Context) ([]StoredEvent, error) {
	if s == nil || s.Client == nil || s.Namespace == "" {
		return nil, ErrStore
	}
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.InNamespace(s.Namespace), client.MatchingLabels{RecordTypeLabel: RecordTypeAudit}); err != nil {
		return nil, ErrStore
	}
	result := make([]StoredEvent, 0, len(secrets.Items))
	for i := range secrets.Items {
		event, err := decodeAuditSecret(&secrets.Items[i])
		if err != nil {
			return nil, ErrStore
		}
		result = append(result, StoredEvent{Name: secrets.Items[i].Name, ResourceVersion: secrets.Items[i].ResourceVersion, Event: event})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp.Equal(result[j].Timestamp) {
			return result[i].Name < result[j].Name
		}
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	return result, nil
}

// SweepBefore exact-deletes at most limit validated events older than cutoff. Validation
// completes before the first delete; malformed protected state therefore fails closed.
func (s *KubernetesStore) SweepBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if cutoff.IsZero() || limit <= 0 {
		return 0, ErrStore
	}
	records, err := s.List(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for i := range records {
		if deleted == limit || !records[i].Timestamp.Before(cutoff) {
			break
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: records[i].Name}}
		resourceVersion := records[i].ResourceVersion
		if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return deleted, ErrStore
		}
		deleted++
	}
	return deleted, nil
}

func decodeAuditSecret(secret *corev1.Secret) (Event, error) {
	if secret == nil || !strings.HasPrefix(secret.Name, auditNamePrefix) || secret.Type != corev1.SecretTypeOpaque ||
		secret.Immutable == nil || !*secret.Immutable || secret.Labels[RecordTypeLabel] != RecordTypeAudit ||
		secret.Labels["app.kubernetes.io/managed-by"] != "sentry-kubernetes-operator" || len(secret.Data) != 1 || len(secret.Data[EventDataKey]) == 0 {
		return Event{}, ErrStore
	}
	var event Event
	if json.Unmarshal(secret.Data[EventDataKey], &event) != nil || Validate(event) != nil {
		return Event{}, ErrStore
	}
	return event, nil
}

func (s *KubernetesStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Round(0)
	}
	return time.Now().UTC().Round(0)
}

var _ Sink = (*KubernetesStore)(nil)
