/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package monitorstate persists protected identities for independently owned monitors.
package monitorstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	stateKey        = "state.json"
	recordTypeLabel = "sentryconfiguration.io/record-type"
	recordType      = "monitor-binding"
	currentVersion  = "v1"
)

var (
	ErrNotFound = errors.New("protected monitor binding not found")
	ErrInvalid  = errors.New("protected monitor binding is invalid")
)

type Reference struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Binding contains no request credential, body, remote response sample, or public status data.
type Binding struct {
	Version          string    `json:"version"`
	Resource         Reference `json:"resource"`
	ProjectUID       string    `json:"projectUID"`
	Connection       string    `json:"connection"`
	Organization     string    `json:"organization"`
	ProjectSlug      string    `json:"projectSlug"`
	ExternalIdentity string    `json:"externalIdentity"`
	RemoteName       string    `json:"remoteName"`
	DesiredHash      string    `json:"desiredHash"`
}

type Record struct {
	Name            string
	ResourceVersion string
	Binding         Binding
}

type Store struct {
	Client    client.Client
	Namespace string
}

func (s *Store) Get(ctx context.Context, uid string) (Record, error) {
	secret := &corev1.Secret{}
	name := bindingName(uid)
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return Record{}, ErrNotFound
		}
		return Record{}, err
	}
	var state Binding
	if secret.Labels[recordTypeLabel] != recordType || json.Unmarshal(secret.Data[stateKey], &state) != nil || validate(state) != nil || state.Resource.UID != uid {
		return Record{}, ErrInvalid
	}
	return Record{Name: name, ResourceVersion: secret.ResourceVersion, Binding: state}, nil
}

func (s *Store) Create(ctx context.Context, state Binding) (Record, error) {
	if err := validate(state); err != nil {
		return Record{}, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return Record{}, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: bindingName(state.Resource.UID), Labels: map[string]string{recordTypeLabel: recordType}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{stateKey: data}}
	if err := s.Client.Create(ctx, secret); err != nil {
		return Record{}, err
	}
	return Record{Name: secret.Name, ResourceVersion: secret.ResourceVersion, Binding: state}, nil
}

func (s *Store) Update(ctx context.Context, previous Record, state Binding) (Record, error) {
	if err := validate(state); err != nil || previous.Name != bindingName(state.Resource.UID) {
		return Record{}, ErrInvalid
	}
	data, err := json.Marshal(state)
	if err != nil {
		return Record{}, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: previous.Name, ResourceVersion: previous.ResourceVersion, Labels: map[string]string{recordTypeLabel: recordType}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{stateKey: data}}
	if err := s.Client.Update(ctx, secret); err != nil {
		return Record{}, err
	}
	return Record{Name: secret.Name, ResourceVersion: secret.ResourceVersion, Binding: state}, nil
}

func (s *Store) Delete(ctx context.Context, previous Record) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: previous.Name}}
	rv := previous.ResourceVersion
	if err := s.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func NewBinding(kind, namespace, name, uid, projectUID, connection, organization, projectSlug, externalIdentity, remoteName, desiredHash string) Binding {
	return Binding{Version: currentVersion, Resource: Reference{Kind: kind, Namespace: namespace, Name: name, UID: uid}, ProjectUID: projectUID, Connection: connection, Organization: organization, ProjectSlug: projectSlug, ExternalIdentity: externalIdentity, RemoteName: remoteName, DesiredHash: desiredHash}
}

func validate(state Binding) error {
	if state.Version != currentVersion || state.Resource.Kind == "" || state.Resource.Namespace == "" || state.Resource.Name == "" || state.Resource.UID == "" || state.ProjectUID == "" || state.Connection == "" || state.Organization == "" || state.ProjectSlug == "" || state.ExternalIdentity == "" || state.RemoteName == "" {
		return ErrInvalid
	}
	digest, err := hex.DecodeString(state.DesiredHash)
	if err != nil || len(digest) != sha256.Size {
		return ErrInvalid
	}
	return nil
}

func bindingName(uid string) string {
	digest := sha256.Sum256([]byte(uid))
	return "sentry-monitor-" + hex.EncodeToString(digest[:])[:32]
}
