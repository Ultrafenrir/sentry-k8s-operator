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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

var (
	errMonitorProjectDeleting               = errors.New("referenced project is deleting")
	errMonitorProjectNotReady               = errors.New("referenced project is not ready")
	errMonitorProjectNotManaged             = errors.New("referenced project is not managed")
	errMonitorConnectionNotReady            = errors.New("referenced connection is not ready")
	errMonitorConnectionRuntimeInitializing = errors.New("referenced connection runtime is initializing")
	errMonitorProtectedStateMismatch        = errors.New("protected project binding does not match connection")
)

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
		return monitorProject{}, errMonitorProjectDeleting
	}
	if project.Status.ObservedGeneration != project.Generation || !meta.IsStatusConditionTrue(project.Status.Conditions, "ProjectReady") {
		return monitorProject{}, errMonitorProjectNotReady
	}
	state, err := bindings.GetBinding(ctx, string(project.UID))
	if err != nil {
		return monitorProject{}, err
	}
	if state.Binding.AccessMode != binding.AccessManage || state.Binding.ExternalIdentity == "" {
		return monitorProject{}, errMonitorProjectNotManaged
	}
	connection := &sentryv1alpha1.SentryConnection{}
	if err := kube.Get(ctx, types.NamespacedName{Name: project.Spec.Sentry}, connection); err != nil {
		return monitorProject{}, err
	}
	apiClient, found := registry.Get(connection.Name)
	if !found {
		if meta.IsStatusConditionTrue(connection.Status.Conditions, "Ready") {
			return monitorProject{}, errMonitorConnectionRuntimeInitializing
		}
		return monitorProject{}, errMonitorConnectionNotReady
	}
	if state.Binding.Connection != connection.Name || state.Binding.Organization != connection.Spec.Organization {
		return monitorProject{}, errMonitorProtectedStateMismatch
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
	switch {
	case errors.Is(err, errMonitorProjectDeleting):
		return "ProjectDeleting"
	case errors.Is(err, errMonitorProjectNotReady):
		return "ProjectNotReady"
	case errors.Is(err, errMonitorProjectNotManaged):
		return "ProjectNotManaged"
	case errors.Is(err, errMonitorConnectionNotReady):
		return "ConnectionNotReady"
	case errors.Is(err, errMonitorProtectedStateMismatch):
		return "ProtectedStateMismatch"
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

func monitorProjectFailureMessage(err error) string {
	switch {
	case errors.Is(err, errMonitorProjectDeleting):
		return "The referenced SentryProject is deleting; monitor reconciliation is paused."
	case errors.Is(err, errMonitorProjectNotReady), errors.Is(err, binding.ErrNotFound):
		return "The referenced SentryProject is not ready yet. Inspect its Ready condition before retrying the monitor."
	case errors.Is(err, errMonitorProjectNotManaged):
		return "The referenced SentryProject is not in managed mode; independently owned monitors require accessMode Manage."
	case errors.Is(err, errMonitorConnectionNotReady):
		return "The referenced SentryConnection is not ready. Inspect its Ready condition for the credential, organization, or network diagnosis."
	case errors.Is(err, errMonitorProtectedStateMismatch):
		return "Protected project state does not match the referenced connection; no Sentry mutation was attempted."
	default:
		return "The referenced managed project could not be resolved. Inspect the project and connection conditions."
	}
}

func readyCondition(generation int64, ready bool, reason, message string) metav1.Condition {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: generation, Reason: reason, Message: message}
}
