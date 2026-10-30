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
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
)

func TestFenceCoordinatorOrdersAndClearsBothStatusFences(t *testing.T) {
	t.Parallel()

	k8sClient, challenge, now := fenceFixture(t)
	recorded := &recordingClient{Client: k8sClient}
	coordinator := &FenceCoordinator{Client: recorded, Now: func() time.Time { return now }}
	operationHash := strings.Repeat("a", 64)
	if err := coordinator.Begin(context.Background(), challenge, operationHash); err != nil {
		t.Fatal(err)
	}
	if got := recorded.order; len(got) != 2 || got[0] != "connection" || got[1] != "project" {
		t.Fatalf("status update order=%v", got)
	}
	assertFenceState(t, k8sClient, challenge, operationHash, true)
	if err := coordinator.Clear(context.Background(), challenge, operationHash); err != nil {
		t.Fatal(err)
	}
	assertFenceState(t, k8sClient, challenge, operationHash, false)
	if got := recorded.order; len(got) != 4 || got[2] != "project" || got[3] != "connection" {
		t.Fatalf("status update order after clear=%v", got)
	}
}

func TestFenceCoordinatorRejectsStalePolicyBeforeAnyStatusWrite(t *testing.T) {
	t.Parallel()

	k8sClient, challenge, now := fenceFixture(t)
	challenge.PolicyResourceVersion = "stale-resource-version"
	recorded := &recordingClient{Client: k8sClient}
	err := (&FenceCoordinator{Client: recorded, Now: func() time.Time { return now }}).Begin(context.Background(), challenge, strings.Repeat("b", 64))
	if !errors.Is(err, ErrFenceStale) {
		t.Fatalf("begin error=%v", err)
	}
	if len(recorded.order) != 0 {
		t.Fatalf("unexpected status writes=%v", recorded.order)
	}
}

func TestFenceCoordinatorDoesNotReplaceLiveForeignFence(t *testing.T) {
	t.Parallel()

	k8sClient, challenge, now := fenceFixture(t)
	coordinator := &FenceCoordinator{Client: k8sClient, Now: func() time.Time { return now }}
	first := strings.Repeat("c", 64)
	if err := coordinator.Begin(context.Background(), challenge, first); err != nil {
		t.Fatal(err)
	}
	connection := &sentryv1alpha1.SentryConnection{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: challenge.Connection}, connection); err != nil {
		t.Fatal(err)
	}
	challenge.PolicyResourceVersion = connection.ResourceVersion
	err := coordinator.Begin(context.Background(), challenge, strings.Repeat("d", 64))
	if !errors.Is(err, ErrFenceConflict) {
		t.Fatalf("second begin error=%v", err)
	}
	assertFenceState(t, k8sClient, challenge, first, true)
}

func TestServiceKeepsFencesUntilResumablePublishCompletes(t *testing.T) {
	t.Parallel()

	k8sClient, challenge, now := fenceFixture(t)
	store := registrationStore(t)
	_, err := store.CreateBinding(context.Background(), binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: challenge.Project, Connection: challenge.Connection,
		Installation: "https://sentry.example", Organization: "company", Slug: "checkout", Owner: challenge.Owner,
		Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: "project-id",
		Marker:     &binding.Marker{Origin: "managed", Owner: challenge.Owner, ManagedBy: "company-op"},
		ManagerUID: challenge.Project.UID, ClaimName: "claim",
		Destinations: map[string]binding.DestinationBinding{"telegram": {
			Kind: "telegram", SelectorHash: challenge.SelectorHash, ResourceName: "payments-alerts",
			State: destination.StateRegistrationRequired,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{fail: true}
	coordinator := &FenceCoordinator{Client: k8sClient, Now: func() time.Time { return now }}
	auditSink := &registrationAuditSink{}
	service := &Service{
		Store: store, Publisher: publisher, Fences: coordinator, Now: func() time.Time { return now },
		Random: deterministicRandom, OperationRandom: deterministicRandom, Audit: auditSink,
	}
	code, err := service.Issue(context.Background(), IssueInput{
		Project: challenge.Project, Destination: challenge.Destination, Owner: challenge.Owner,
		Connection: challenge.Connection, PolicyGeneration: challenge.PolicyGeneration,
		PolicyResourceVersion: challenge.PolicyResourceVersion, TTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	auditSink.fail = true
	if err := service.Redeem(context.Background(), challenge.Project.UID, challenge.Destination, code, binding.TelegramTarget{ChatIdentity: "private-chat"}, nil); !errors.Is(err, ErrAuditUnavailable) {
		t.Fatalf("audit outage error=%v", err)
	}
	assertFenceState(t, k8sClient, challenge, "", false)
	record, err := store.GetBinding(context.Background(), challenge.Project.UID)
	if err != nil {
		t.Fatal(err)
	}
	if publisher.count() != 0 || record.Binding.Destinations[challenge.Destination].Telegram.Target != nil {
		t.Fatalf("audit outage mutated registration: publishes=%d binding=%#v", publisher.count(), record.Binding.Destinations[challenge.Destination])
	}
	auditSink.fail = false
	if err := service.Redeem(context.Background(), challenge.Project.UID, challenge.Destination, code, binding.TelegramTarget{ChatIdentity: "private-chat"}, nil); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("redeem error=%v", err)
	}
	record, err = store.GetBinding(context.Background(), challenge.Project.UID)
	if err != nil {
		t.Fatal(err)
	}
	telegram := record.Binding.Destinations[challenge.Destination].Telegram
	if telegram.Phase != PhasePendingPublish || telegram.FenceHash == "" {
		t.Fatalf("pending journal=%#v", telegram)
	}
	assertFenceState(t, k8sClient, challenge, telegram.FenceHash, true)

	publisher.fail = false
	if err := service.Resume(context.Background(), challenge.Project.UID, challenge.Destination); err != nil {
		t.Fatal(err)
	}
	record, _ = store.GetBinding(context.Background(), challenge.Project.UID)
	telegram = record.Binding.Destinations[challenge.Destination].Telegram
	if record.Binding.Destinations[challenge.Destination].State != destination.StateActive || telegram.Phase != "" || telegram.FenceHash != "" || telegram.Challenge != nil {
		t.Fatalf("completed journal=%#v", record.Binding.Destinations[challenge.Destination])
	}
	assertFenceState(t, k8sClient, challenge, "", false)
	if len(auditSink.events) != 4 || auditSink.events[2].Reason != audit.ReasonResumeRetry || auditSink.events[3].Outcome != audit.OutcomeRecovered {
		t.Fatalf("audit recovery events=%#v", auditSink.events)
	}
}

func fenceFixture(t *testing.T) (client.Client, binding.TelegramChallenge, time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "company", UID: types.UID("connection-uid"), Generation: 7, ResourceVersion: "1"},
		Spec: sentryv1alpha1.SentryConnectionSpec{
			URL: "https://sentry.example", Organization: "company",
			Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{Name: "token", Key: "token"}},
			Access: []sentryv1alpha1.AccessRule{{
				Owner: "payments", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments"}},
				Teams: []string{"payments"}, AllowCreate: true,
				Destinations: &sentryv1alpha1.DestinationAccess{Telegram: &sentryv1alpha1.TelegramDestinationGrant{Targets: []string{"payments-alerts"}}},
			}},
		},
	}
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("project-uid"), Generation: 3, ResourceVersion: "1"},
		Spec: sentryv1alpha1.SentryProjectSpec{
			Connection: "company", Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments", Slug: "checkout"}},
			Destinations: map[string]sentryv1alpha1.DestinationSpec{"telegram": {Telegram: &sentryv1alpha1.TelegramDestination{Target: "payments-alerts"}}},
		},
		Status: sentryv1alpha1.SentryProjectStatus{Destinations: map[string]sentryv1alpha1.DestinationStatus{
			"telegram": {Type: "telegram", State: destination.StateRegistrationRequired, Reason: "TelegramRegistrationRequired"},
		}},
	}
	_, selectorHash, err := destination.SelectorFingerprint(connection.Spec, project.Spec.Destinations["telegram"])
	if err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&sentryv1alpha1.SentryConnection{}, &sentryv1alpha1.SentryProject{}).
		WithObjects(connection, project).Build()
	challenge := binding.TelegramChallenge{
		Project:     binding.ProjectReference{Namespace: project.Namespace, Name: project.Name, UID: string(project.UID), Generation: project.Generation},
		Destination: "telegram", SelectorHash: selectorHash, Owner: "payments", Connection: connection.Name,
		PolicyGeneration: connection.Generation, PolicyResourceVersion: connection.ResourceVersion, ExpiresAt: now.Add(10 * time.Minute),
	}
	return k8sClient, challenge, now
}

func assertFenceState(t *testing.T, k8sClient client.Client, challenge binding.TelegramChallenge, operationHash string, present bool) {
	t.Helper()
	connection := &sentryv1alpha1.SentryConnection{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: challenge.Connection}, connection); err != nil {
		t.Fatal(err)
	}
	project := &sentryv1alpha1.SentryProject{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: challenge.Project.Namespace, Name: challenge.Project.Name}, project); err != nil {
		t.Fatal(err)
	}
	projectCommit := project.Status.Destinations[challenge.Destination].RegistrationCommit
	if present {
		if connection.Status.RegistrationCommit == nil || connection.Status.RegistrationCommit.OperationHash != operationHash || projectCommit == nil || projectCommit.OperationHash != operationHash {
			t.Fatalf("connection commit=%#v project commit=%#v", connection.Status.RegistrationCommit, projectCommit)
		}
		return
	}
	if connection.Status.RegistrationCommit != nil || projectCommit != nil {
		t.Fatalf("fences remain: connection=%#v project=%#v", connection.Status.RegistrationCommit, projectCommit)
	}
}

type recordingClient struct {
	client.Client
	order []string
}

func (c *recordingClient) Status() client.SubResourceWriter {
	return &recordingStatusWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type recordingStatusWriter struct {
	client.SubResourceWriter
	client *recordingClient
}

func (w *recordingStatusWriter) Update(ctx context.Context, object client.Object, opts ...client.SubResourceUpdateOption) error {
	switch object.(type) {
	case *sentryv1alpha1.SentryConnection:
		w.client.order = append(w.client.order, "connection")
	case *sentryv1alpha1.SentryProject:
		w.client.order = append(w.client.order, "project")
	}
	return w.SubResourceWriter.Update(ctx, object, opts...)
}
