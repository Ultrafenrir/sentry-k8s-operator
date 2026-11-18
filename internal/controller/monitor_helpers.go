/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type monitorProject struct {
	Project    *sentryv1alpha1.SentryProject
	Connection *sentryv1alpha1.SentryConnection
	Client     *sentryapi.Client
	Binding    binding.BindingRecord
}

func recordMonitorAudit(ctx context.Context, sink audit.Sink, action audit.Action, local string, generation int64) error {
	if sink == nil {
		return nil
	}
	if generation < 1 {
		generation = 1
	}
	return sink.Record(ctx, audit.Event{Timestamp: time.Now().UTC().Round(0), Component: audit.ComponentMonitors, Action: action, Outcome: audit.OutcomeSucceeded, Reason: audit.ReasonMutationVerified, LocalResource: local, Revision: generation})
}

func resolveMonitorProject(ctx context.Context, kube client.Client, registry *connectionruntime.Registry, bindings *binding.Store, namespace, name string) (monitorProject, error) {
	project := &sentryv1alpha1.SentryProject{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, project); err != nil {
		return monitorProject{}, err
	}
	if !project.DeletionTimestamp.IsZero() {
		return monitorProject{}, fmt.Errorf("project is deleting")
	}
	state, err := bindings.GetBinding(ctx, string(project.UID))
	if err != nil {
		return monitorProject{}, err
	}
	if state.Binding.AccessMode != binding.AccessManage || state.Binding.ExternalIdentity == "" {
		return monitorProject{}, fmt.Errorf("project is not managed")
	}
	connection := &sentryv1alpha1.SentryConnection{}
	if err := kube.Get(ctx, types.NamespacedName{Name: project.Spec.Sentry}, connection); err != nil {
		return monitorProject{}, err
	}
	apiClient, found := registry.Get(connection.Name)
	if !found {
		return monitorProject{}, fmt.Errorf("connection is not ready")
	}
	if state.Binding.Connection != connection.Name || state.Binding.Organization != connection.Spec.Organization {
		return monitorProject{}, fmt.Errorf("protected project binding does not match connection")
	}
	return monitorProject{Project: project, Connection: connection, Client: apiClient, Binding: state}, nil
}

func desiredDigest(value any) string {
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func monitorMarker(uid types.UID) string {
	digest := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(digest[:])[:12]
}

func monitorRemoteName(display, localName string, uid types.UID) string {
	if strings.TrimSpace(display) == "" {
		display = localName
	}
	suffix := " [managed:" + monitorMarker(uid) + "]"
	limit := 128 - len(suffix)
	if len(display) > limit {
		display = display[:limit]
	}
	return display + suffix
}

func monitorDeletionPending(identity, expectedIdentity, status string) bool {
	return identity == expectedIdentity && status == "pending_deletion"
}

func monitorReason(err error) string {
	if err == nil {
		return "MonitorReady"
	}
	if apierrors.IsNotFound(err) {
		return "ProjectNotFound"
	}
	if errors.Is(err, binding.ErrNotFound) {
		return "ProjectNotReady"
	}
	var apiErr *sentryapi.APIError
	if sentryapi.AsAPIError(err, &apiErr) {
		switch apiErr.Kind {
		case sentryapi.ErrorUnauthorized, sentryapi.ErrorPermissionDenied:
			return "SentryAuthorizationFailed"
		case sentryapi.ErrorInvalid, sentryapi.ErrorConflict:
			return "SentryRejected"
		case sentryapi.ErrorNotFound:
			return "RemoteObjectMissing"
		case sentryapi.ErrorRateLimited:
			return "SentryRateLimited"
		default:
			return "SentryUnavailable"
		}
	}
	return "ReconciliationFailed"
}

func readyCondition(generation int64, ready bool, reason, message string) metav1.Condition {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: generation, Reason: reason, Message: message}
}
