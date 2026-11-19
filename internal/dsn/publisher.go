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
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

var (
	ErrSecretConflict = errors.New("the namespace DSN output Secret has an incompatible type")
	ErrAuthorization  = errors.New("DSN output authorization changed before write")
)

type Phase string

const (
	PhaseReady Phase = "Ready"
)

type Publisher struct {
	Client            client.Client
	AllowInsecureHTTP bool
}

type Authorization func(context.Context) error

// Reconcile creates the fixed-name namespace Secret when missing and writes only
// the key named after the SentryProject. Other projects' keys and all metadata are preserved.
func (p *Publisher) Reconcile(ctx context.Context, project *sentryv1alpha1.SentryProject, publicDSN string, current binding.DSNBinding, authorize Authorization) (binding.DSNBinding, Phase, error) {
	if !validPublicDSN(publicDSN, p.AllowInsecureHTTP) {
		return current, "", ErrInvalidDSN
	}
	if err := p.ensureKey(ctx, project, publicDSN, authorize); err != nil {
		return current, "", err
	}
	current.CurrentSecret = sentryv1alpha1.DSNSecretName
	current.PendingSecret = ""
	return current, PhaseReady, nil
}

// Revoke removes only this project's key after a caller has proven policy revoke.
func (p *Publisher) Revoke(ctx context.Context, project *sentryv1alpha1.SentryProject, current binding.DSNBinding, authorize Authorization) error {
	if current.CurrentSecret == "" {
		return nil
	}
	return p.deleteKey(ctx, project, authorize)
}

func (p *Publisher) ensureKey(ctx context.Context, project *sentryv1alpha1.SentryProject, value string, authorize Authorization) error {
	secret := &corev1.Secret{}
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: sentryv1alpha1.DSNSecretName}, secret)
	if apierrors.IsNotFound(err) {
		if err := authorize(ctx); err != nil {
			return ErrAuthorization
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: project.Namespace, Name: sentryv1alpha1.DSNSecretName},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{project.Name: []byte(value)},
		}
		if err := p.Client.Create(ctx, secret); err == nil {
			return nil
		} else if !apierrors.IsAlreadyExists(err) {
			return err
		}
		secret = &corev1.Secret{}
		err = p.Client.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: sentryv1alpha1.DSNSecretName}, secret)
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	if secret.Type != corev1.SecretTypeOpaque && secret.Type != "" {
		return ErrSecretConflict
	}
	if string(secret.Data[project.Name]) == value {
		return nil
	}
	if err := authorize(ctx); err != nil {
		return ErrAuthorization
	}
	base := secret.DeepCopy()
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[project.Name] = []byte(value)
	return p.Client.Patch(ctx, secret, client.MergeFrom(base))
}

func (p *Publisher) deleteKey(ctx context.Context, project *sentryv1alpha1.SentryProject, authorize Authorization) error {
	secret := &corev1.Secret{}
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: sentryv1alpha1.DSNSecretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if secret.Type != corev1.SecretTypeOpaque && secret.Type != "" {
		return ErrSecretConflict
	}
	if _, found := secret.Data[project.Name]; !found {
		return nil
	}
	if err := authorize(ctx); err != nil {
		return ErrAuthorization
	}
	base := secret.DeepCopy()
	delete(secret.Data, project.Name)
	return p.Client.Patch(ctx, secret, client.MergeFrom(base))
}
