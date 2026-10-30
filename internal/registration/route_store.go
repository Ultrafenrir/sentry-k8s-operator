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

package registration

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	routeRecordKey   = "route.json"
	routeRecordLabel = "telegram-route"
)

var (
	ErrRouteInvalid      = errors.New("telegram route record is invalid")
	ErrRouteConflict     = errors.New("telegram route idempotency key is already bound to another target")
	ErrRouteIdentityLost = errors.New("telegram route key is missing from protected binding")
)

type routeRecord struct {
	Version   string                 `json:"version"`
	OutboxKey string                 `json:"outboxKey"`
	Target    binding.TelegramTarget `json:"target"`
}

// RouteStore is an idempotent durable Publisher backed by immutable Secrets in the
// dedicated protected-state namespace. It is a routing-record backend, not the delivery
// queue; at-rest encryption remains a cluster installation requirement.
type RouteStore struct {
	Client    client.Client
	Namespace string
}

func (s *RouteStore) PublishTelegramRoute(ctx context.Context, outboxKey string, target binding.TelegramTarget) error {
	if s.Client == nil || s.Namespace == "" || !validOperationHash(outboxKey) || !validTelegramTarget(target) {
		return ErrRouteInvalid
	}
	record := routeRecord{Version: binding.CurrentVersion, OutboxKey: outboxKey, Target: target}
	payload, err := json.Marshal(record)
	if err != nil {
		return ErrRouteInvalid
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Namespace,
			Name:      routeName(outboxKey),
			Labels:    map[string]string{recordTypeLabel: routeRecordLabel},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{routeRecordKey: payload},
	}
	if err := s.Client.Create(ctx, secret); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	current, err := s.GetTelegramRoute(ctx, outboxKey)
	if err != nil {
		return err
	}
	if current != target {
		return ErrRouteConflict
	}
	return nil
}

func (s *RouteStore) GetTelegramRoute(ctx context.Context, outboxKey string) (binding.TelegramTarget, error) {
	if s.Client == nil || s.Namespace == "" || !validOperationHash(outboxKey) {
		return binding.TelegramTarget{}, ErrRouteInvalid
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: routeName(outboxKey)}, secret); err != nil {
		return binding.TelegramTarget{}, err
	}
	return decodeRouteSecret(secret, outboxKey)
}

func (s *RouteStore) RevokeTelegramRoute(ctx context.Context, outboxKey string, expected binding.TelegramTarget) error {
	if s.Client == nil || s.Namespace == "" || !validOperationHash(outboxKey) || !validTelegramTarget(expected) {
		return ErrRouteInvalid
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: routeName(outboxKey)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	target, err := decodeRouteSecret(secret, outboxKey)
	if err != nil {
		return err
	}
	if target != expected {
		return ErrRouteConflict
	}
	resourceVersion := secret.ResourceVersion
	if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func decodeRouteSecret(secret *corev1.Secret, outboxKey string) (binding.TelegramTarget, error) {
	record, err := decodeRouteRecord(secret)
	if err != nil || record.OutboxKey != outboxKey {
		return binding.TelegramTarget{}, ErrRouteInvalid
	}
	return record.Target, nil
}

func decodeRouteRecord(secret *corev1.Secret) (routeRecord, error) {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Labels[recordTypeLabel] != routeRecordLabel || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 1 {
		return routeRecord{}, ErrRouteInvalid
	}
	var record routeRecord
	if err := json.Unmarshal(secret.Data[routeRecordKey], &record); err != nil || record.Version != binding.CurrentVersion || !validOperationHash(record.OutboxKey) || secret.Name != routeName(record.OutboxKey) || !validTelegramTarget(record.Target) {
		return routeRecord{}, ErrRouteInvalid
	}
	return record, nil
}

// ValidateRoutePayload validates a protected route payload and deterministic object name
// without requiring a Kubernetes client.
func ValidateRoutePayload(name string, payload []byte) error {
	var record routeRecord
	if json.Unmarshal(payload, &record) != nil || record.Version != binding.CurrentVersion || !validOperationHash(record.OutboxKey) || name != routeName(record.OutboxKey) || !validTelegramTarget(record.Target) {
		return ErrRouteInvalid
	}
	return nil
}

// MigrateLegacy publishes immutable routes from a former protected-state
// namespace before exact-deleting their source records. It is intended for the
// leader manager identity; the registration writer has no source list access.
func (s *RouteStore) MigrateLegacy(ctx context.Context, sourceNamespace string, limit int) (int, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || sourceNamespace == "" || sourceNamespace == s.Namespace || limit <= 0 {
		return 0, ErrRouteInvalid
	}
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.InNamespace(sourceNamespace), client.MatchingLabels{recordTypeLabel: routeRecordLabel}); err != nil {
		return 0, err
	}
	sort.Slice(secrets.Items, func(i, j int) bool { return secrets.Items[i].Name < secrets.Items[j].Name })
	migrated := 0
	var migrationErr error
	for i := range secrets.Items {
		if migrated == limit {
			break
		}
		record, err := decodeRouteRecord(&secrets.Items[i])
		if err != nil {
			migrationErr = errors.Join(migrationErr, ErrRouteInvalid)
			continue
		}
		if err := s.PublishTelegramRoute(ctx, record.OutboxKey, record.Target); err != nil {
			migrationErr = errors.Join(migrationErr, err)
			continue
		}
		resourceVersion := secrets.Items[i].ResourceVersion
		if err := s.Client.Delete(ctx, &secrets.Items[i], client.Preconditions{ResourceVersion: &resourceVersion}); err != nil {
			if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				migrationErr = errors.Join(migrationErr, err)
			}
			continue
		}
		migrated++
	}
	return migrated, migrationErr
}

func validTelegramTarget(target binding.TelegramTarget) bool {
	chatID, err := strconv.ParseInt(target.ChatIdentity, 10, 64)
	if err != nil || chatID == 0 || len(target.ChatIdentity) > 128 || len(target.TopicIdentity) > 128 {
		return false
	}
	if target.TopicIdentity == "" {
		return true
	}
	topicID, err := strconv.ParseInt(target.TopicIdentity, 10, 64)
	return err == nil && topicID > 0
}

func routeName(outboxKey string) string {
	return "sentry-telegram-route-" + outboxKey
}

const recordTypeLabel = "sentryconfiguration.io/record-type"

var _ Publisher = (*RouteStore)(nil)
var _ RouteManager = (*RouteStore)(nil)
