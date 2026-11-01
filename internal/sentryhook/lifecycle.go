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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

var ErrServiceHookLifecycle = errors.New("Sentry service-hook lifecycle failed")

type LifecycleVerifierStore interface {
	VerifierPublicationStore
	Get(context.Context, string) ([]byte, error)
	CompareAndSwap(context.Context, string, []byte, []byte) error
}

type LifecycleEngine struct {
	Bindings *binding.Store
	Adapter  ServiceHookAdapter
	Sources  SourcePublicationStore
	Verifier LifecycleVerifierStore
}

type LifecycleInput struct {
	Binding       binding.BindingRecord
	ConnectionUID string
	ReceiverBase  string
	Organization  string
	ProjectSlug   string
	Revision      int64
	Deliveries    []TelegramDelivery
	Authorize     func(context.Context) error
}

func (e *LifecycleEngine) Ensure(ctx context.Context, input LifecycleInput) (binding.BindingRecord, error) {
	if e == nil || e.Bindings == nil || e.Adapter == nil || e.Sources == nil || e.Verifier == nil ||
		input.Binding.Binding.Project.UID == "" || input.Organization != input.Binding.Binding.Organization || input.ProjectSlug != input.Binding.Binding.Slug || input.Revision < 1 || input.Authorize == nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	sourceBindingKey, err := DeriveSourceBindingKey(input.ConnectionUID, input.Binding.Binding.Project.UID)
	if err != nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	endpoint, err := SourceEndpointURL(input.ReceiverBase, sourceBindingKey)
	if err != nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	plan := SourcePlan{SourceBindingKey: sourceBindingKey, ExpectedProjectSlug: input.ProjectSlug, Deliveries: input.Deliveries}
	record := input.Binding
	protected := record.Binding.ServiceHook
	if protected == nil {
		next := record.Binding
		next.ServiceHook = &binding.ServiceHookBinding{
			SourceBindingKey: sourceBindingKey, EndpointURL: endpoint, Revision: input.Revision,
			Pending: &binding.OperationState{Kind: "CreateServiceHook", Phase: string(sentryapi.OperationSubmitted), DesiredHash: serviceHookDesiredHash(endpoint)},
		}
		record, err = e.Bindings.UpdateBinding(ctx, record, next)
		if err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		protected = record.Binding.ServiceHook
	}
	if protected.SourceBindingKey != sourceBindingKey || protected.Revision > input.Revision {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	if protected.ExternalIdentity == "" {
		if protected.EndpointURL != endpoint || protected.Revision != input.Revision {
			return binding.BindingRecord{}, ErrServiceHookLifecycle
		}
		remote, resolveErr := e.createOrRecover(ctx, input.Organization, input.ProjectSlug, endpoint, input.Authorize)
		if resolveErr != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, resolveErr)
		}
		if err := e.publish(ctx, input.Organization, input.ProjectSlug, plan, nil, remote); err != nil {
			return binding.BindingRecord{}, err
		}
		if !sameDesiredServiceHook(remote, endpoint) {
			if err := input.Authorize(ctx); err != nil {
				return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
			}
			if _, err := e.Adapter.Repair(ctx, input.Organization, input.ProjectSlug, remote.Identity, endpoint); err != nil {
				return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
			}
			remote, _, err = e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, remote.Identity)
			if err != nil || !sameDesiredServiceHook(remote, endpoint) {
				return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
			}
		}
		next := record.Binding
		next.ServiceHook = &binding.ServiceHookBinding{SourceBindingKey: sourceBindingKey, ExternalIdentity: remote.Identity, EndpointURL: endpoint, SecretHash: serviceHookSecretHash(remote.Secret), Revision: input.Revision}
		updated, updateErr := e.Bindings.UpdateBinding(ctx, record, next)
		if updateErr != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, updateErr)
		}
		return updated, nil
	}
	if protected.Pending != nil && protected.Pending.Kind == "RotateServiceHook" {
		resumeInput := input
		resumeInput.Revision = protected.TargetRevision
		return e.rotate(ctx, resumeInput, record, plan, protected.TargetEndpointURL)
	}
	if protected.Revision < input.Revision {
		return e.rotate(ctx, input, record, plan, endpoint)
	}
	if protected.EndpointURL != endpoint || protected.Pending != nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	remote, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
	if err != nil || !found {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if remote.URL != endpoint || !remote.Active || len(remote.Events) != 1 || remote.Events[0] != AlertEvent {
		if err := input.Authorize(ctx); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		remote, err = e.Adapter.Repair(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity, endpoint)
		if err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		remote, found, err = e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		if err != nil || !found || remote.URL != endpoint || !remote.Active || len(remote.Events) != 1 || remote.Events[0] != AlertEvent {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	if err := e.publish(ctx, input.Organization, input.ProjectSlug, plan, protected, remote); err != nil {
		return binding.BindingRecord{}, err
	}
	if protected.SecretHash == serviceHookSecretHash(remote.Secret) {
		return record, nil
	}
	next := record.Binding
	next.ServiceHook = &binding.ServiceHookBinding{SourceBindingKey: sourceBindingKey, ExternalIdentity: remote.Identity, EndpointURL: endpoint, SecretHash: serviceHookSecretHash(remote.Secret), Revision: input.Revision}
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	return updated, nil
}

func (e *LifecycleEngine) Remove(ctx context.Context, input LifecycleInput) (binding.BindingRecord, error) {
	if e == nil || e.Bindings == nil || e.Adapter == nil || e.Sources == nil || e.Verifier == nil || input.Binding.Binding.Project.UID == "" || input.Organization != input.Binding.Binding.Organization || input.ProjectSlug != input.Binding.Binding.Slug || input.Authorize == nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	record := input.Binding
	protected := record.Binding.ServiceHook
	if protected == nil {
		return record, nil
	}
	sourceBindingKey, err := DeriveSourceBindingKey(input.ConnectionUID, record.Binding.Project.UID)
	if err != nil || protected.SourceBindingKey != sourceBindingKey {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	endpoint, err := SourceEndpointURL(input.ReceiverBase, sourceBindingKey)
	if err != nil || protected.EndpointURL != endpoint || protected.ExternalIdentity == "" {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	if protected.Pending == nil {
		next := record.Binding
		nextHook := *protected
		nextHook.Pending = &binding.OperationState{Kind: "DeleteServiceHook", Phase: string(sentryapi.OperationSubmitted), DesiredHash: serviceHookDesiredHash(endpoint)}
		next.ServiceHook = &nextHook
		record, err = e.Bindings.UpdateBinding(ctx, record, next)
		if err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		protected = record.Binding.ServiceHook
	}
	if err := e.unpublish(ctx, input.ProjectSlug, protected.SourceBindingKey); err != nil {
		return binding.BindingRecord{}, err
	}
	remote, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if found {
		if remote.Identity != protected.ExternalIdentity {
			return binding.BindingRecord{}, ErrServiceHookLifecycle
		}
		if err := input.Authorize(ctx); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		deleteErr := e.Adapter.Delete(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		verified, stillFound, verifyErr := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		_ = verified
		if verifyErr != nil || stillFound {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, deleteErr, verifyErr)
		}
	}
	next := record.Binding
	next.ServiceHook = nil
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	return updated, nil
}

func (e *LifecycleEngine) rotate(ctx context.Context, input LifecycleInput, record binding.BindingRecord, plan SourcePlan, endpoint string) (binding.BindingRecord, error) {
	protected := record.Binding.ServiceHook
	if protected == nil || protected.ExternalIdentity == "" {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	if protected.Pending == nil {
		next := record.Binding
		nextHook := *protected
		nextHook.Pending = &binding.OperationState{Kind: "RotateServiceHook", Phase: string(sentryapi.OperationSubmitted), DesiredHash: serviceHookRotationHash(endpoint, input.Revision)}
		nextHook.TargetEndpointURL = endpoint
		nextHook.TargetRevision = input.Revision
		next.ServiceHook = &nextHook
		var err error
		record, err = e.Bindings.UpdateBinding(ctx, record, next)
		if err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		protected = record.Binding.ServiceHook
	}
	if protected.Pending.Kind != "RotateServiceHook" || protected.TargetRevision != input.Revision || protected.TargetEndpointURL != endpoint {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	replacement, err := e.resolveReplacement(ctx, input, protected)
	if err != nil {
		return binding.BindingRecord{}, err
	}
	if protected.ReplacementIdentity == "" {
		next := record.Binding
		nextHook := *protected
		nextHook.ReplacementIdentity = replacement.Identity
		next.ServiceHook = &nextHook
		record, err = e.Bindings.UpdateBinding(ctx, record, next)
		if err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		protected = record.Binding.ServiceHook
	}
	oldRemote, oldFound, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if oldFound && !sameDesiredServiceHook(oldRemote, protected.EndpointURL) {
		if err := input.Authorize(ctx); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		if _, err := e.Adapter.Repair(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity, protected.EndpointURL); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		oldRemote, oldFound, err = e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		if err != nil || !oldFound || !sameDesiredServiceHook(oldRemote, protected.EndpointURL) {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	current, err := e.Verifier.Get(ctx, protected.SourceBindingKey)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	overlap, retired, err := PlanVerifierRotation(current, replacement.Secret)
	if err != nil {
		return binding.BindingRecord{}, ErrServiceHookLifecycle
	}
	if oldFound {
		overlap, err = EncodeVerifierConfig(input.ProjectSlug, [][]byte{oldRemote.Secret, replacement.Secret})
		if err != nil {
			return binding.BindingRecord{}, ErrServiceHookLifecycle
		}
	}
	publisher := &Publisher{Sources: e.Sources, Verifier: e.Verifier}
	if _, err := publisher.Publish(ctx, plan, current); err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if !bytes.Equal(current, overlap) {
		if err := e.Verifier.CompareAndSwap(ctx, protected.SourceBindingKey, current, overlap); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	if !sameDesiredServiceHook(replacement, endpoint) {
		if err := input.Authorize(ctx); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		if _, err := e.Adapter.Repair(ctx, input.Organization, input.ProjectSlug, replacement.Identity, endpoint); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	replacement, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, replacement.Identity)
	if err != nil || !found || !sameDesiredServiceHook(replacement, endpoint) {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if oldFound {
		if err := input.Authorize(ctx); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		deleteErr := e.Adapter.Delete(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		_, stillFound, verifyErr := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ExternalIdentity)
		if verifyErr != nil || stillFound {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, deleteErr, verifyErr)
		}
	}
	latestPayload, err := e.Verifier.Get(ctx, protected.SourceBindingKey)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	if !bytes.Equal(latestPayload, retired) {
		if !bytes.Equal(latestPayload, overlap) {
			return binding.BindingRecord{}, ErrServiceHookLifecycle
		}
		if err := e.Verifier.CompareAndSwap(ctx, protected.SourceBindingKey, overlap, retired); err != nil {
			return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	next := record.Binding
	next.ServiceHook = &binding.ServiceHookBinding{
		SourceBindingKey: protected.SourceBindingKey, ExternalIdentity: replacement.Identity,
		EndpointURL: endpoint, SecretHash: serviceHookSecretHash(replacement.Secret), Revision: input.Revision,
	}
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return binding.BindingRecord{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	return updated, nil
}

func (e *LifecycleEngine) resolveReplacement(ctx context.Context, input LifecycleInput, protected *binding.ServiceHookBinding) (RemoteServiceHook, error) {
	stagingEndpoint, err := stagingServiceHookEndpoint(protected.TargetEndpointURL)
	if err != nil {
		return RemoteServiceHook{}, ErrServiceHookLifecycle
	}
	if protected.ReplacementIdentity != "" {
		remote, found, getErr := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, protected.ReplacementIdentity)
		if getErr != nil || !found || !validRecoverableServiceHookLocation(remote, protected.TargetEndpointURL, stagingEndpoint) {
			return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, getErr)
		}
		return remote, nil
	}
	hooks, err := e.Adapter.List(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	matching := make([]RemoteServiceHook, 0, 1)
	for _, hook := range hooks {
		if hook.Identity != protected.ExternalIdentity && (hook.URL == protected.TargetEndpointURL || hook.URL == stagingEndpoint) {
			matching = append(matching, hook)
		}
	}
	if len(matching) > 1 {
		return RemoteServiceHook{}, ErrServiceHookLifecycle
	}
	if len(matching) == 0 {
		if err := input.Authorize(ctx); err != nil {
			return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		created, err := e.Adapter.Create(ctx, input.Organization, input.ProjectSlug, stagingEndpoint, false)
		if err != nil {
			return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, err)
		}
		matching = append(matching, created)
	}
	remote, found, err := e.Adapter.Get(ctx, input.Organization, input.ProjectSlug, matching[0].Identity)
	if err != nil || !found || !validRecoverableServiceHookLocation(remote, protected.TargetEndpointURL, stagingEndpoint) {
		return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	return remote, nil
}

func (e *LifecycleEngine) unpublish(ctx context.Context, projectSlug, sourceBindingKey string) error {
	payload, verifierErr := e.Verifier.Get(ctx, sourceBindingKey)
	verifierMissing := apierrors.IsNotFound(verifierErr)
	if verifierErr != nil && !verifierMissing {
		return errors.Join(ErrServiceHookLifecycle, verifierErr)
	}
	source, sourceErr := e.Sources.Get(ctx, sourceBindingKey)
	sourceMissing := apierrors.IsNotFound(sourceErr)
	if sourceErr != nil && !sourceMissing {
		return errors.Join(ErrServiceHookLifecycle, sourceErr)
	}
	if !verifierMissing && !sourceMissing {
		configuration, _, err := decodeVerifierConfig(payload)
		if err != nil || configuration.ExpectedProjectSlug != projectSlug {
			return ErrServiceHookLifecycle
		}
		if err := (&Publisher{Sources: e.Sources, Verifier: e.Verifier}).Unpublish(ctx, source, payload); err != nil {
			return errors.Join(ErrServiceHookLifecycle, err)
		}
		return nil
	}
	if !verifierMissing {
		if err := e.Verifier.DeleteExact(ctx, sourceBindingKey, payload); err != nil {
			return errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	if !sourceMissing {
		if err := e.Sources.DeleteExact(ctx, source); err != nil && !apierrors.IsNotFound(err) {
			return errors.Join(ErrServiceHookLifecycle, err)
		}
	}
	return nil
}

func (e *LifecycleEngine) createOrRecover(ctx context.Context, organization, project, endpoint string, authorize func(context.Context) error) (RemoteServiceHook, error) {
	stagingEndpoint, err := stagingServiceHookEndpoint(endpoint)
	if err != nil {
		return RemoteServiceHook{}, ErrServiceHookLifecycle
	}
	hooks, err := e.Adapter.List(ctx, organization, project)
	if err != nil {
		return RemoteServiceHook{}, err
	}
	matching := make([]RemoteServiceHook, 0, 1)
	for _, hook := range hooks {
		if hook.URL == endpoint || hook.URL == stagingEndpoint {
			matching = append(matching, hook)
		}
	}
	if len(matching) > 1 {
		return RemoteServiceHook{}, ErrServiceHookLifecycle
	}
	if len(matching) == 0 {
		if err := authorize(ctx); err != nil {
			return RemoteServiceHook{}, err
		}
		created, createErr := e.Adapter.Create(ctx, organization, project, stagingEndpoint, false)
		if createErr != nil {
			return RemoteServiceHook{}, createErr
		}
		matching = append(matching, created)
	}
	remote, found, err := e.Adapter.Get(ctx, organization, project, matching[0].Identity)
	if err != nil || !found || !validRecoverableServiceHookLocation(remote, endpoint, stagingEndpoint) {
		return RemoteServiceHook{}, errors.Join(ErrServiceHookLifecycle, err)
	}
	return remote, nil
}

func (e *LifecycleEngine) publish(ctx context.Context, organization, project string, plan SourcePlan, protected *binding.ServiceHookBinding, remote RemoteServiceHook) error {
	endpoint := protectedEndpoint(protected, remote.URL)
	if (protected == nil && !validRecoverableServiceHook(remote, endpoint)) || (protected != nil && !sameDesiredServiceHook(remote, endpoint)) {
		return ErrServiceHookLifecycle
	}
	publisher := &Publisher{Sources: e.Sources, Verifier: e.Verifier}
	retired, err := EncodeVerifierConfig(plan.ExpectedProjectSlug, [][]byte{remote.Secret})
	if err != nil {
		return ErrServiceHookLifecycle
	}
	if protected == nil || protected.SecretHash == serviceHookSecretHash(remote.Secret) {
		if _, err := publisher.Publish(ctx, plan, retired); err != nil {
			return errors.Join(ErrServiceHookLifecycle, err)
		}
		return nil
	}
	current, err := e.Verifier.Get(ctx, plan.SourceBindingKey)
	if apierrors.IsNotFound(err) {
		if _, err = publisher.Publish(ctx, plan, retired); err != nil {
			return errors.Join(ErrServiceHookLifecycle, err)
		}
		return nil
	}
	if err != nil {
		return errors.Join(ErrServiceHookLifecycle, err)
	}
	overlap, plannedRetired, err := PlanVerifierRotation(current, remote.Secret)
	if err != nil || !bytes.Equal(plannedRetired, retired) {
		return ErrServiceHookLifecycle
	}
	if _, err := publisher.Publish(ctx, plan, overlap); err != nil {
		return errors.Join(ErrServiceHookLifecycle, err)
	}
	verified, found, err := e.Adapter.Get(ctx, organization, project, remote.Identity)
	if err != nil || !found || verified.Identity != remote.Identity || !bytes.Equal(verified.Secret, remote.Secret) {
		return errors.Join(ErrServiceHookLifecycle, err)
	}
	if err := e.Verifier.CompareAndSwap(ctx, plan.SourceBindingKey, overlap, retired); err != nil {
		return errors.Join(ErrServiceHookLifecycle, err)
	}
	return nil
}

func sameDesiredServiceHook(remote RemoteServiceHook, endpoint string) bool {
	return remote.Identity != "" && remote.URL == endpoint && remote.Active && len(remote.Events) == 1 && remote.Events[0] == AlertEvent && len(remote.Secret) >= 16
}

func validRecoverableServiceHook(remote RemoteServiceHook, endpoint string) bool {
	return remote.Identity != "" && remote.URL == endpoint && len(remote.Events) == 1 && remote.Events[0] == AlertEvent && len(remote.Secret) >= 16
}

func validRecoverableServiceHookLocation(remote RemoteServiceHook, endpoint, stagingEndpoint string) bool {
	return validRecoverableServiceHook(remote, endpoint) || validRecoverableServiceHook(remote, stagingEndpoint)
}

func stagingServiceHookEndpoint(endpoint string) (string, error) {
	if !validServiceHookEndpoint(endpoint) {
		return "", ErrServiceHookLifecycle
	}
	digest := sha256.Sum256([]byte("staging-service-hook\x00" + endpoint))
	return endpoint[:len(endpoint)-64] + hex.EncodeToString(digest[:]), nil
}

func protectedEndpoint(protected *binding.ServiceHookBinding, fallback string) string {
	if protected == nil {
		return fallback
	}
	return protected.EndpointURL
}

func serviceHookSecretHash(secret []byte) string {
	digest := sha256.Sum256(secret)
	return hex.EncodeToString(digest[:])
}

func serviceHookDesiredHash(endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint + "\x00" + AlertEvent))
	return hex.EncodeToString(digest[:])
}

func serviceHookRotationHash(endpoint string, revision int64) string {
	digest := sha256.Sum256([]byte(endpoint + "\x00" + AlertEvent + "\x00" + fmt.Sprint(revision)))
	return hex.EncodeToString(digest[:])
}

var _ LifecycleVerifierStore = (*VerifierStore)(nil)
