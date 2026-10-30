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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	challengeIndexKey   = "challenge.json"
	challengeIndexLabel = "telegram-challenge-index"
)

var ErrChallengeIndexInvalid = errors.New("telegram challenge index is invalid")

// ChallengeIndex contains no raw one-time code or Telegram target. It lets the
// registration writer resolve hash(code) with one exact Secret GET and therefore
// does not require list/watch over protected state.
type ChallengeIndex struct {
	Version     string    `json:"version"`
	CodeHash    string    `json:"codeHash"`
	ProjectUID  string    `json:"projectUID"`
	Destination string    `json:"destination"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type ChallengeIndexStore struct {
	Client    client.Client
	Namespace string
}

func (s *ChallengeIndexStore) Create(ctx context.Context, record ChallengeIndex) error {
	if !s.valid(record) {
		return ErrChallengeIndexInvalid
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return ErrChallengeIndexInvalid
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Namespace,
			Name:      challengeIndexName(record.CodeHash),
			Labels:    map[string]string{recordTypeLabel: challengeIndexLabel},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{challengeIndexKey: payload},
	}
	return s.Client.Create(ctx, secret)
}

func (s *ChallengeIndexStore) Get(ctx context.Context, codeHash string) (ChallengeIndex, error) {
	if s.Client == nil || s.Namespace == "" || !validOperationHash(codeHash) {
		return ChallengeIndex{}, ErrChallengeIndexInvalid
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: challengeIndexName(codeHash)}, secret); err != nil {
		return ChallengeIndex{}, err
	}
	return s.decode(secret, codeHash)
}

// DeleteExact removes only the immutable record that still matches every
// security-relevant field. This avoids deleting a colliding or replaced record.
func (s *ChallengeIndexStore) DeleteExact(ctx context.Context, expected ChallengeIndex) error {
	if !s.valid(expected) {
		return ErrChallengeIndexInvalid
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: challengeIndexName(expected.CodeHash)}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	current, err := s.decode(secret, expected.CodeHash)
	if err != nil || !sameChallengeIndex(current, expected) {
		return ErrChallengeIndexInvalid
	}
	resourceVersion := secret.ResourceVersion
	if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// SweepExpired is reserved for the leader-elected controller identity. The
// registration writer deliberately has no list permission and cannot call it.
// Malformed labelled records are never deleted; valid expired records can still
// be collected and the malformed-record error is returned for visibility.
func (s *ChallengeIndexStore) SweepExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if s == nil || s.Client == nil || s.Namespace == "" || now.IsZero() || limit <= 0 {
		return 0, ErrChallengeIndexInvalid
	}
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.InNamespace(s.Namespace), client.MatchingLabels{recordTypeLabel: challengeIndexLabel}); err != nil {
		return 0, err
	}
	type candidate struct {
		record ChallengeIndex
		secret corev1.Secret
	}
	candidates := make([]candidate, 0, len(secrets.Items))
	var sweepErr error
	for i := range secrets.Items {
		record, err := s.decode(&secrets.Items[i], "")
		if err != nil {
			sweepErr = errors.Join(sweepErr, ErrChallengeIndexInvalid)
			continue
		}
		if !record.ExpiresAt.After(now) {
			candidates = append(candidates, candidate{record: record, secret: secrets.Items[i]})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].record.ExpiresAt.Equal(candidates[j].record.ExpiresAt) {
			return candidates[i].secret.Name < candidates[j].secret.Name
		}
		return candidates[i].record.ExpiresAt.Before(candidates[j].record.ExpiresAt)
	})
	removed := 0
	for i := range candidates {
		if removed == limit {
			break
		}
		resourceVersion := candidates[i].secret.ResourceVersion
		if err := s.Client.Delete(ctx, &candidates[i].secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
				continue
			}
			sweepErr = errors.Join(sweepErr, err)
			continue
		}
		removed++
	}
	return removed, sweepErr
}

func (s *ChallengeIndexStore) decode(secret *corev1.Secret, expectedHash string) (ChallengeIndex, error) {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Labels[recordTypeLabel] != challengeIndexLabel || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 1 {
		return ChallengeIndex{}, ErrChallengeIndexInvalid
	}
	var record ChallengeIndex
	if err := json.Unmarshal(secret.Data[challengeIndexKey], &record); err != nil || (expectedHash != "" && record.CodeHash != expectedHash) || secret.Name != challengeIndexName(record.CodeHash) || !s.valid(record) {
		return ChallengeIndex{}, ErrChallengeIndexInvalid
	}
	return record, nil
}

func sameChallengeIndex(left, right ChallengeIndex) bool {
	return left.Version == right.Version && left.CodeHash == right.CodeHash && left.ProjectUID == right.ProjectUID &&
		left.Destination == right.Destination && left.ExpiresAt.Equal(right.ExpiresAt)
}

func (s *ChallengeIndexStore) valid(record ChallengeIndex) bool {
	return s != nil && s.Client != nil && s.Namespace != "" && validChallengeIndex(record)
}

func validChallengeIndex(record ChallengeIndex) bool {
	return record.Version == binding.CurrentVersion && validOperationHash(record.CodeHash) && record.ProjectUID != "" && len(record.ProjectUID) <= 256 && record.Destination != "" && len(record.Destination) <= 63 && !record.ExpiresAt.IsZero()
}

// ValidateChallengeIndexPayload validates a protected backup payload and its deterministic
// Kubernetes object name without requiring a client.
func ValidateChallengeIndexPayload(name string, payload []byte) error {
	var record ChallengeIndex
	if json.Unmarshal(payload, &record) != nil || name != challengeIndexName(record.CodeHash) || !validChallengeIndex(record) {
		return ErrChallengeIndexInvalid
	}
	return nil
}

func challengeIndexName(codeHash string) string {
	return "sentry-telegram-challenge-" + codeHash
}
