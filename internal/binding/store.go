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

package binding

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

const (
	stateKey        = "state.json"
	recordTypeLabel = "sentryconfiguration.io/record-type"
	recordBinding   = "project-binding"
	recordClaim     = "manager-claim"
	recordOperation = "operation-journal"
)

var (
	ErrNotFound          = errors.New("protected record not found")
	ErrOwnershipConflict = errors.New("manager claim is held by another SentryProject")
	ErrInvalidRecord     = errors.New("protected record is invalid")
	ErrHandoffDenied     = errors.New("manager handoff authorization does not match target")
)

// Store persists protected records as Opaque Secrets in a dedicated state namespace.
type Store struct {
	Client    client.Client
	Namespace string
}

// Load implements sentryapi.OperationJournal using a protected Secret record.
func (s *Store) Load(ctx context.Context, key string) (sentryapi.Operation, bool, error) {
	name := OperationName(key)
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return sentryapi.Operation{}, false, nil
		}
		return sentryapi.Operation{}, false, err
	}
	var operation sentryapi.Operation
	if err := decodeSecret(secret, recordOperation, &operation); err != nil || operation.Key != key || validateOperation(operation) != nil {
		return sentryapi.Operation{}, false, ErrInvalidRecord
	}
	return operation, true, nil
}

// CompareAndSwap implements sentryapi.OperationJournal with payload and Kubernetes CAS.
func (s *Store) CompareAndSwap(ctx context.Context, previousRevision int64, next sentryapi.Operation) error {
	if validateOperation(next) != nil || next.Revision != previousRevision+1 {
		return ErrInvalidRecord
	}
	name := OperationName(next.Key)
	if previousRevision == 0 {
		if next.Phase != sentryapi.OperationPlanned {
			return ErrInvalidRecord
		}
		secret, err := encodeSecret(s.Namespace, name, recordOperation, next)
		if err != nil {
			return err
		}
		return s.Client.Create(ctx, secret)
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, secret); err != nil {
		return err
	}
	var current sentryapi.Operation
	if err := decodeSecret(secret, recordOperation, &current); err != nil || validateOperation(current) != nil || current.Key != next.Key || current.Revision != previousRevision || !validOperationTransition(current, next) {
		return ErrInvalidRecord
	}
	replacement, err := encodeSecret(s.Namespace, name, recordOperation, next)
	if err != nil {
		return err
	}
	replacement.ResourceVersion = secret.ResourceVersion
	return s.Client.Update(ctx, replacement)
}

func validateOperation(operation sentryapi.Operation) error {
	if operation.Key == "" || len(operation.Key) > 1024 || operation.Kind == "" || len(operation.Kind) > 128 || operation.LocalResource == "" || len(operation.LocalResource) > 512 || strings.ContainsAny(operation.Key+operation.Kind+operation.LocalResource, "\r\n\x00") || operation.Revision <= 0 {
		return ErrInvalidRecord
	}
	digest, err := hex.DecodeString(operation.IdempotencyHash)
	if err != nil || len(digest) < 2 || len(digest) > 64 {
		return ErrInvalidRecord
	}
	switch operation.Phase {
	case sentryapi.OperationPlanned, sentryapi.OperationSubmitted, sentryapi.OperationVerified, sentryapi.OperationFailed:
		return nil
	default:
		return ErrInvalidRecord
	}
}

func validOperationTransition(current, next sentryapi.Operation) bool {
	if current.Key != next.Key || current.Kind != next.Kind || current.LocalResource != next.LocalResource || next.Revision != current.Revision+1 {
		return false
	}
	switch current.Phase {
	case sentryapi.OperationPlanned:
		return next.Phase == sentryapi.OperationPlanned || (next.Phase == sentryapi.OperationSubmitted && next.IdempotencyHash == current.IdempotencyHash)
	case sentryapi.OperationSubmitted:
		return (next.Phase == sentryapi.OperationVerified || next.Phase == sentryapi.OperationFailed) && next.IdempotencyHash == current.IdempotencyHash
	case sentryapi.OperationVerified, sentryapi.OperationFailed:
		return next.Phase == sentryapi.OperationPlanned
	default:
		return false
	}
}

func (s *Store) CreateBinding(ctx context.Context, binding ProjectBinding) (BindingRecord, error) {
	if err := validateBinding(binding); err != nil {
		return BindingRecord{}, err
	}
	name := BindingName(binding.Project.UID)
	secret, err := encodeSecret(s.Namespace, name, recordBinding, binding)
	if err != nil {
		return BindingRecord{}, err
	}
	if err := s.Client.Create(ctx, secret); err != nil {
		return BindingRecord{}, err
	}
	return BindingRecord{Name: name, ResourceVersion: secret.ResourceVersion, Binding: binding}, nil
}

func (s *Store) GetBinding(ctx context.Context, projectUID string) (BindingRecord, error) {
	name := BindingName(projectUID)
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return BindingRecord{}, ErrNotFound
		}
		return BindingRecord{}, err
	}
	var binding ProjectBinding
	if err := decodeSecret(secret, recordBinding, &binding); err != nil || validateBinding(binding) != nil || binding.Project.UID != projectUID {
		return BindingRecord{}, ErrInvalidRecord
	}
	return BindingRecord{Name: name, ResourceVersion: secret.ResourceVersion, Binding: binding}, nil
}

// HasPolicyChildren reports whether any protected project binding still contains an
// alert or notification target owned by the exact policy incarnation. Invalid protected
// state is an error so a policy finalizer fails closed instead of orphaning Sentry objects.
func (s *Store) HasPolicyChildren(ctx context.Context, policyUID string) (bool, error) {
	if policyUID == "" {
		return false, ErrInvalidRecord
	}
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.InNamespace(s.Namespace), client.MatchingLabels{recordTypeLabel: recordBinding}); err != nil {
		return false, err
	}
	for i := range secrets.Items {
		var project ProjectBinding
		if err := decodeSecret(&secrets.Items[i], recordBinding, &project); err != nil || validateBinding(project) != nil {
			return false, ErrInvalidRecord
		}
		for _, target := range project.Destinations {
			if target.Owner.UID == policyUID {
				return true, nil
			}
		}
		for _, alert := range project.Alerts {
			if alert.Owner.UID == policyUID {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Store) UpdateBinding(ctx context.Context, previous BindingRecord, next ProjectBinding) (BindingRecord, error) {
	if err := validateBinding(next); err != nil || previous.Name != BindingName(next.Project.UID) {
		return BindingRecord{}, ErrInvalidRecord
	}
	secret, err := encodeSecret(s.Namespace, previous.Name, recordBinding, next)
	if err != nil {
		return BindingRecord{}, err
	}
	secret.ResourceVersion = previous.ResourceVersion
	if err := s.Client.Update(ctx, secret); err != nil {
		return BindingRecord{}, err
	}
	return BindingRecord{Name: previous.Name, ResourceVersion: secret.ResourceVersion, Binding: next}, nil
}

func (s *Store) DeleteBinding(ctx context.Context, record BindingRecord) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: record.Name}}
	resourceVersion := record.ResourceVersion
	if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// FindManaged proves origin only from a matching protected binding. Marker must be checked separately.
func (s *Store) FindManaged(ctx context.Context, installation, organization, slug, externalIdentity string, observedMarker Marker) (BindingRecord, bool, error) {
	secrets := &corev1.SecretList{}
	if err := s.Client.List(ctx, secrets, client.InNamespace(s.Namespace), client.MatchingLabels{recordTypeLabel: recordBinding}); err != nil {
		return BindingRecord{}, false, err
	}
	var found BindingRecord
	matches := 0
	for i := range secrets.Items {
		var binding ProjectBinding
		if err := decodeSecret(&secrets.Items[i], recordBinding, &binding); err != nil || validateBinding(binding) != nil {
			continue
		}
		if binding.Origin == OriginManaged && binding.AccessMode == AccessManage && binding.Installation == installation && binding.Organization == organization && binding.Slug == slug && binding.ExternalIdentity == externalIdentity && binding.Marker != nil && binding.Marker.Matches(observedMarker) {
			found = BindingRecord{Name: secrets.Items[i].Name, ResourceVersion: secrets.Items[i].ResourceVersion, Binding: binding}
			matches++
		}
	}
	return found, matches >= 1, nil
}

// AcquireClaim uses Kubernetes Create as the global serialization point before external mutation.
func (s *Store) AcquireClaim(ctx context.Context, claim ManagerClaim) (ClaimRecord, bool, error) {
	if err := validateClaim(claim); err != nil {
		return ClaimRecord{}, false, err
	}
	name := ClaimName(claim.Installation, claim.Organization, claim.Slug)
	secret, err := encodeSecret(s.Namespace, name, recordClaim, claim)
	if err != nil {
		return ClaimRecord{}, false, err
	}
	if err := s.Client.Create(ctx, secret); err == nil {
		return ClaimRecord{Name: name, ResourceVersion: secret.ResourceVersion, Claim: claim}, true, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return ClaimRecord{}, false, err
	}
	current, err := s.GetClaim(ctx, claim.Installation, claim.Organization, claim.Slug)
	if err != nil {
		return ClaimRecord{}, false, err
	}
	if current.Claim.Manager.UID != claim.Manager.UID {
		return ClaimRecord{}, false, ErrOwnershipConflict
	}
	return current, false, nil
}

func (s *Store) GetClaim(ctx context.Context, installation, organization, slug string) (ClaimRecord, error) {
	name := ClaimName(installation, organization, slug)
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return ClaimRecord{}, ErrNotFound
		}
		return ClaimRecord{}, err
	}
	var claim ManagerClaim
	if err := decodeSecret(secret, recordClaim, &claim); err != nil || validateClaim(claim) != nil || ClaimName(claim.Installation, claim.Organization, claim.Slug) != name {
		return ClaimRecord{}, ErrInvalidRecord
	}
	return ClaimRecord{Name: name, ResourceVersion: secret.ResourceVersion, Claim: claim}, nil
}

func (s *Store) UpdateClaim(ctx context.Context, previous ClaimRecord, next ManagerClaim) (ClaimRecord, error) {
	if err := validateClaim(next); err != nil || previous.Name != ClaimName(next.Installation, next.Organization, next.Slug) {
		return ClaimRecord{}, ErrInvalidRecord
	}
	secret, err := encodeSecret(s.Namespace, previous.Name, recordClaim, next)
	if err != nil {
		return ClaimRecord{}, err
	}
	secret.ResourceVersion = previous.ResourceVersion
	if err := s.Client.Update(ctx, secret); err != nil {
		return ClaimRecord{}, err
	}
	return ClaimRecord{Name: previous.Name, ResourceVersion: secret.ResourceVersion, Claim: next}, nil
}

func (s *Store) DeleteClaim(ctx context.Context, record ClaimRecord) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: record.Name}}
	resourceVersion := record.ResourceVersion
	if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion}); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Handoff atomically changes manager and creates one exact target authorization.
// Its caller is responsible for admin authorization and external ownership verification.
func (s *Store) Handoff(ctx context.Context, previous ClaimRecord, sourceUID string, target ProjectReference) (ClaimRecord, error) {
	if previous.Claim.Manager.UID != sourceUID || target.UID == "" || target.Generation <= 0 || target.Name == "" || target.Namespace == "" || target.UID == sourceUID || previous.Claim.Slug == "" {
		return ClaimRecord{}, ErrHandoffDenied
	}
	next := previous.Claim
	next.Manager = target
	next.Handoff = &HandoffAuthorization{Target: target}
	return s.UpdateClaim(ctx, previous, next)
}

func (s *Store) AuthorizeHandoff(record ClaimRecord, target ProjectReference, slug string) error {
	if record.Claim.Handoff == nil || record.Claim.Manager.UID != target.UID || record.Claim.Handoff.Target != target || record.Claim.Slug != slug {
		return ErrHandoffDenied
	}
	return nil
}

func (s *Store) CompleteHandoff(ctx context.Context, previous ClaimRecord, target ProjectReference, slug string) (ClaimRecord, error) {
	if err := s.AuthorizeHandoff(previous, target, slug); err != nil {
		return ClaimRecord{}, err
	}
	next := previous.Claim
	next.Handoff = nil
	return s.UpdateClaim(ctx, previous, next)
}

func encodeSecret(namespace, name, recordType string, value any) (*corev1.Secret, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sentry-kubernetes-operator",
				recordTypeLabel:                recordType,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{stateKey: payload},
	}, nil
}

func decodeSecret(secret *corev1.Secret, recordType string, target any) error {
	if secret.Type != corev1.SecretTypeOpaque || secret.Labels[recordTypeLabel] != recordType || len(secret.Data[stateKey]) == 0 {
		return ErrInvalidRecord
	}
	if err := json.Unmarshal(secret.Data[stateKey], target); err != nil {
		return fmt.Errorf("%w: malformed state", ErrInvalidRecord)
	}
	return nil
}

func validateBinding(binding ProjectBinding) error {
	if binding.Version != CurrentVersion || binding.Project.UID == "" || binding.Project.Name == "" || binding.Project.Namespace == "" || binding.Installation == "" || binding.Organization == "" || binding.Slug == "" {
		return ErrInvalidRecord
	}
	if binding.Origin == OriginManaged {
		if binding.Owner == "" || binding.Marker == nil || binding.Marker.Origin != "managed" || binding.Marker.Owner != binding.Owner {
			return ErrInvalidRecord
		}
		if binding.AccessMode == AccessManage && (binding.ManagerUID == "" || binding.ClaimName == "") {
			return ErrInvalidRecord
		}
		if binding.AccessMode == AccessReadOnly && (binding.ManagerUID != "" || binding.ClaimName != "") {
			return ErrInvalidRecord
		}
		if binding.AccessMode != AccessManage && binding.AccessMode != AccessReadOnly {
			return ErrInvalidRecord
		}
	} else if binding.Origin != OriginExternal || binding.AccessMode != AccessReadOnly || binding.Owner != "" || binding.ManagerUID != "" || binding.Marker != nil || binding.ClaimName != "" {
		return ErrInvalidRecord
	}
	if err := validateAlertBindings(binding); err != nil {
		return err
	}
	for key, target := range binding.Destinations {
		if !ValidNotificationTargetKey(key) || !target.Owner.Valid() {
			return ErrInvalidRecord
		}
	}
	return nil
}

// ValidateProjectBinding validates decoded protected state without performing a Kubernetes
// read or write. Backup and recovery tooling uses it to reject checksum-valid corruption.
func ValidateProjectBinding(value ProjectBinding) error {
	return validateBinding(value)
}

func validateAlertBindings(project ProjectBinding) error {
	if len(project.Alerts) == 0 {
		return nil
	}
	if len(project.Alerts) > 100 || project.Origin != OriginManaged || project.AccessMode != AccessManage || project.Marker == nil {
		return ErrInvalidRecord
	}
	pinned := project.PinnedAdapters["alerts"]
	if pinned == "" {
		return ErrInvalidRecord
	}
	for key, alert := range project.Alerts {
		if len(validation.IsDNS1123Label(key)) != 0 || !alert.Owner.Valid() || alert.Adapter != pinned || alert.ExternalIdentity == "" || len(alert.ExternalIdentity) > 512 || strings.ContainsAny(alert.ExternalIdentity, "\r\n\x00") || alert.Trigger == "" || len(alert.Trigger) > 64 || !alert.Marker.Matches(*project.Marker) {
			return ErrInvalidRecord
		}
		digest, err := hex.DecodeString(alert.DesiredHash)
		if err != nil || len(digest) != 32 || len(alert.Destinations) == 0 || len(alert.Destinations) > 20 {
			return ErrInvalidRecord
		}
		seen := make(map[string]bool, len(alert.Destinations))
		for _, destination := range alert.Destinations {
			if !ValidNotificationTargetKey(destination) || seen[destination] {
				return ErrInvalidRecord
			}
			seen[destination] = true
		}
	}
	return nil
}

// ValidNotificationTargetKey validates the canonical provider/readable-name key used by
// protected bindings and registration requests.
func ValidNotificationTargetKey(value string) bool {
	provider, name, found := strings.Cut(value, "/")
	if !found || name == "" || len(value) > 211 || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\r\n\x00") {
		return false
	}
	return provider == "slack" || provider == "pagerDuty" || provider == "telegram"
}

func validateClaim(claim ManagerClaim) error {
	if claim.Version != CurrentVersion || claim.Installation == "" || claim.Organization == "" || claim.Slug == "" || claim.Owner == "" || claim.Manager.UID == "" || claim.Manager.Name == "" || claim.Manager.Namespace == "" {
		return ErrInvalidRecord
	}
	return nil
}

// ValidateManagerClaim validates decoded protected manager state.
func ValidateManagerClaim(value ManagerClaim) error {
	return validateClaim(value)
}

// ValidateOperation validates decoded protected journal state.
func ValidateOperation(value sentryapi.Operation) error {
	return validateOperation(value)
}
