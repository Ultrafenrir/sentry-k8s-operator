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

package dsn

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const ConfirmCutoverAnnotation = "sentryconfiguration.io/confirm-dsn-cutover"

var (
	ErrSecretConflict    = errors.New("DSN output Secret is not owned by this SentryProject")
	ErrCutoverInProgress = errors.New("another DSN Secret cutover is already pending")
	ErrAuthorization     = errors.New("DSN output authorization changed before write")
	ErrInvalidSecretName = errors.New("DSN output Secret name is invalid")
)

type Phase string

const (
	PhaseReady          Phase = "Ready"
	PhaseCutoverPending Phase = "CutoverPending"
)

type Publisher struct {
	Client            client.Client
	AllowInsecureHTTP bool
}

type Authorization func(context.Context) error

// Reconcile creates/restores a managed Secret and performs an explicitly confirmed rename.
func (p *Publisher) Reconcile(ctx context.Context, project *sentryv1alpha1.SentryProject, owner, desiredName, publicDSN string, current binding.DSNBinding, authorize Authorization) (binding.DSNBinding, Phase, error) {
	if desiredName == "" {
		desiredName = project.Name + sentryv1alpha1.DefaultDSNSecretSuffix
	}
	if problems := validation.IsDNS1123Subdomain(desiredName); len(problems) > 0 {
		return current, "", ErrInvalidSecretName
	}
	if !validPublicDSN(publicDSN, p.AllowInsecureHTTP) {
		return current, "", ErrInvalidDSN
	}
	if current.CurrentSecret == "" {
		if err := p.ensureSecret(ctx, project, owner, desiredName, publicDSN, authorize); err != nil {
			return current, "", err
		}
		current.CurrentSecret = desiredName
		return current, PhaseReady, nil
	}
	if current.PendingSecret != "" && current.PendingSecret != desiredName {
		return current, "", ErrCutoverInProgress
	}
	if err := p.ensureSecret(ctx, project, owner, current.CurrentSecret, publicDSN, authorize); err != nil {
		return current, "", err
	}
	if desiredName == current.CurrentSecret {
		current.PendingSecret = ""
		return current, PhaseReady, nil
	}
	if err := p.ensureSecret(ctx, project, owner, desiredName, publicDSN, authorize); err != nil {
		return current, "", err
	}
	current.PendingSecret = desiredName
	if project.Annotations[ConfirmCutoverAnnotation] != desiredName {
		return current, PhaseCutoverPending, nil
	}
	if err := p.deleteSecret(ctx, project, current.CurrentSecret, authorize); err != nil {
		return current, "", err
	}
	current.CurrentSecret = desiredName
	current.PendingSecret = ""
	return current, PhaseReady, nil
}

// Revoke removes only exact managed output Secrets after a caller has proven policy revoke.
func (p *Publisher) Revoke(ctx context.Context, project *sentryv1alpha1.SentryProject, current binding.DSNBinding, authorize Authorization) error {
	if current.PendingSecret != "" && current.PendingSecret != current.CurrentSecret {
		if err := p.deleteSecret(ctx, project, current.PendingSecret, authorize); err != nil {
			return err
		}
	}
	if current.CurrentSecret != "" {
		return p.deleteSecret(ctx, project, current.CurrentSecret, authorize)
	}
	return nil
}

func (p *Publisher) ensureSecret(ctx context.Context, project *sentryv1alpha1.SentryProject, owner, name, value string, authorize Authorization) error {
	secret := &corev1.Secret{}
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		if err := authorize(ctx); err != nil {
			return ErrAuthorization
		}
		return p.Client.Create(ctx, desiredSecret(project, owner, name, value))
	}
	if err != nil {
		return err
	}
	if !ownedBy(secret, project) {
		return ErrSecretConflict
	}
	if string(secret.Data[sentryv1alpha1.DefaultDSNSecretKey]) == value && len(secret.Data) == 1 && secret.Type == corev1.SecretTypeOpaque && secret.Labels["sentryconfiguration.io/owner"] == owner {
		return nil
	}
	if err := authorize(ctx); err != nil {
		return ErrAuthorization
	}
	base := secret.DeepCopy()
	desired := desiredSecret(project, owner, name, value)
	secret.Labels = desired.Labels
	secret.Annotations = desired.Annotations
	secret.OwnerReferences = desired.OwnerReferences
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = desired.Data
	return p.Client.Patch(ctx, secret, client.MergeFrom(base))
}

func (p *Publisher) deleteSecret(ctx context.Context, project *sentryv1alpha1.SentryProject, name string, authorize Authorization) error {
	secret := &corev1.Secret{}
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedBy(secret, project) {
		return ErrSecretConflict
	}
	if err := authorize(ctx); err != nil {
		return ErrAuthorization
	}
	return p.Client.Delete(ctx, secret, client.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion})
}

func desiredSecret(project *sentryv1alpha1.SentryProject, owner, name, value string) *corev1.Secret {
	controller := true
	blockOwnerDeletion := true
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "sentry-kubernetes-operator",
	}
	if owner != "" {
		labels["sentryconfiguration.io/owner"] = owner
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   project.Namespace,
			Name:        name,
			Labels:      labels,
			Annotations: map[string]string{"sentryconfiguration.io/project-uid": string(project.UID)},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         sentryv1alpha1.GroupVersion.String(),
				Kind:               "SentryProject",
				Name:               project.Name,
				UID:                project.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{sentryv1alpha1.DefaultDSNSecretKey: []byte(value)},
	}
}

func ownedBy(secret *corev1.Secret, project *sentryv1alpha1.SentryProject) bool {
	if secret.Labels["app.kubernetes.io/managed-by"] != "sentry-kubernetes-operator" || secret.Annotations["sentryconfiguration.io/project-uid"] != string(project.UID) {
		return false
	}
	for _, reference := range secret.OwnerReferences {
		if reference.Controller != nil && *reference.Controller && reference.UID == project.UID && reference.Name == project.Name && reference.Kind == "SentryProject" {
			return true
		}
	}
	return false
}
