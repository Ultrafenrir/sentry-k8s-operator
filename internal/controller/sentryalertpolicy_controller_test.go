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

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/dsn"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/ownership"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestPolicyConsumerRolloutRequiresPolicyChildrenToBeReady(t *testing.T) {
	t.Parallel()

	policy := &sentryv1alpha1.SentryAlertPolicy{Spec: sentryv1alpha1.SentryAlertPolicySpec{
		NotificationTargets: &sentryv1alpha1.NotificationTargets{PagerDuty: map[string][]string{"company": {"Backend Primary"}}},
		Alerts:              map[string]sentryv1alpha1.AlertSpec{"volume": {Trigger: sentryv1alpha1.AlertTriggerEventCount}},
	}}
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: sentryv1alpha1.SentryProjectStatus{
			Conditions: []metav1.Condition{{Type: "AlertPolicyReady", Status: metav1.ConditionTrue, ObservedGeneration: 2}},
			NotificationTargets: map[string]sentryv1alpha1.DestinationStatus{
				"pagerDuty/Backend Primary": {Type: "pagerduty", State: destination.StateActive},
			},
			Alerts: map[string]sentryv1alpha1.AlertStatus{"volume": {State: alerts.StateReady}},
		},
	}
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionTrue {
		t.Fatalf("rollout status = %s, want True", got)
	}
	project.Status.Alerts["volume"] = sentryv1alpha1.AlertStatus{State: alerts.StateError}
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionFalse {
		t.Fatalf("failed child rollout status = %s, want False", got)
	}
	delete(project.Status.Alerts, "volume")
	if got := policyConsumerRolloutStatus(policy, project); got != metav1.ConditionUnknown {
		t.Fatalf("missing child rollout status = %s, want Unknown", got)
	}
}

func TestProjectDriftReconcileIntervalRemainsPrompt(t *testing.T) {
	t.Parallel()
	if projectDriftReconcileInterval < 10*time.Second || projectDriftReconcileInterval > time.Minute {
		t.Fatalf("project drift reconcile interval = %s, want a bounded interval from 10s through 1m", projectDriftReconcileInterval)
	}
}

func TestAlertPolicyPendingRolloutIsRequeued(t *testing.T) {
	t.Parallel()

	result := alertPolicyRolloutResult(1)
	if result.RequeueAfter != policyRolloutRequeueInterval {
		t.Fatalf("pending rollout requeue = %s, want %s", result.RequeueAfter, policyRolloutRequeueInterval)
	}
	if result := alertPolicyRolloutResult(0); result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("complete rollout unexpectedly requeued: %#v", result)
	}
}

func TestConnectionMutationFenceDelegatesActiveRevisionVerification(t *testing.T) {
	t.Parallel()
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec: sentryv1alpha1.SentryConnectionSpec{Ownership: &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}},
	}
	if reason, message := connectionMutationFence(connection); reason != "" || message != "" {
		t.Fatalf("active fence = (%q, %q)", reason, message)
	}
	connection.Status.ObservedGeneration = 3
	connection.Status.Region = "eu-west-1"
	connection.Status.EffectiveRole = string(sentryv1alpha1.ConnectionRoleActive)
	connection.Status.ObservedRevision = 8
	connection.Status.Conditions = []metav1.Condition{{
		Type: "OwnershipReady", Status: metav1.ConditionTrue, Reason: "RevisionVerified", ObservedGeneration: 3,
	}}
	if reason, message := connectionMutationFence(connection); reason != "" || message != "" {
		t.Fatalf("verified fence = (%q, %q)", reason, message)
	}
	connection.Spec.Ownership.Role = sentryv1alpha1.ConnectionRoleStandby
	if reason, _ := connectionMutationFence(connection); reason != "ConnectionStandby" {
		t.Fatalf("standby reason = %q", reason)
	}
}

func TestRegionalRevisionAuthorizerRevalidatesRemoteEvidence(t *testing.T) {
	t.Parallel()
	marker := ownership.Marker{Region: "eu-west-1", Revision: 8, Owner: "payments", Installation: "company-sentry-op"}
	evidence, err := ownership.EvidenceFor(marker)
	if err != nil {
		t.Fatal(err)
	}
	registry := connectionruntime.NewRegistry()
	if _, _, err := registry.Apply("primary", "https://sentry.example.test", connectionruntime.SecretInputs{Token: []byte("test")}); err != nil {
		t.Fatal(err)
	}
	adapter := &revisionEvidenceAdapter{remote: projectruntime.RemoteProject{Identity: "project-42", Slug: "checkout", RegionalEvidence: &evidence}}
	reconciler := &SentryProjectReconciler{
		Registry: registry,
		NewAdapter: func(*sentryapi.Client) projectruntime.Adapter {
			return adapter
		},
	}
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "primary"},
		Spec: sentryv1alpha1.SentryConnectionSpec{Ownership: &sentryv1alpha1.ConnectionOwnership{
			Region: marker.Region, Role: sentryv1alpha1.ConnectionRoleActive, Revision: marker.Revision,
		}},
	}
	protected := binding.ProjectBinding{
		Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: adapter.remote.Identity,
		Organization: "company", Slug: adapter.remote.Slug, Owner: marker.Owner,
		Installation: "https://sentry.example.test", Marker: &binding.Marker{Origin: "managed", Owner: marker.Owner, ManagedBy: marker.Installation}, RegionalOwnership: &marker,
	}
	if err := reconciler.validateRegionalRevision(context.Background(), connection, protected); err != nil {
		t.Fatalf("valid regional evidence rejected: %v", err)
	}
	protected.AccessMode = binding.AccessReadOnly
	if err := reconciler.validateChildMutationOwnership(context.Background(), connection, protected); err != nil {
		t.Fatalf("managed standby child evidence rejected: %v", err)
	}
	changed := evidence
	changed.Revision++
	adapter.remote.RegionalEvidence = &changed
	if err := reconciler.validateChildMutationOwnership(context.Background(), connection, protected); !errors.Is(err, dsn.ErrAuthorization) {
		t.Fatalf("changed evidence error = %v, want authorization denial", err)
	}
	connection.Spec.Ownership.Role = sentryv1alpha1.ConnectionRoleStandby
	adapter.remote.RegionalEvidence = &evidence
	if err := reconciler.validateChildMutationOwnership(context.Background(), connection, protected); !errors.Is(err, dsn.ErrAuthorization) {
		t.Fatalf("standby error = %v, want authorization denial", err)
	}
}

type revisionEvidenceAdapter struct {
	remote projectruntime.RemoteProject
}

func (a *revisionEvidenceAdapter) GetBySlug(context.Context, string, string) (projectruntime.RemoteProject, error) {
	return a.remote, nil
}

func (a *revisionEvidenceAdapter) Create(context.Context, projectruntime.CreateRequest) (projectruntime.RemoteProject, error) {
	return projectruntime.RemoteProject{}, errors.New("unexpected create")
}

func (a *revisionEvidenceAdapter) Delete(context.Context, string, string) error {
	return errors.New("unexpected delete")
}
