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

package statebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

func TestProtectedStateRoundTripAgainstAPIServer(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest API server: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest API server: %v", err)
		}
	})
	config.QPS = 100
	config.Burst = 200
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	namespaces := Namespaces{State: "backup-state", Routing: "backup-routing", Delivery: "backup-delivery", HookSources: "backup-hook-sources"}
	for _, name := range protectedNamespaces(namespaces) {
		if err := kubernetesClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	project := binding.ProjectReference{Namespace: "checkout", Name: "api", UID: "project-uid", Generation: 3}
	marker := &binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-operator"}
	bindingStore := &binding.Store{Client: kubernetesClient, Namespace: namespaces.State}
	projectBinding := binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: project, Connection: "company", Installation: "https://sentry.example.test",
		Organization: "company", Slug: "checkout-api", Owner: "payments", Origin: binding.OriginManaged,
		AccessMode: binding.AccessManage, ExternalIdentity: "external-project-id", Marker: marker,
		ManagerUID: project.UID, ClaimName: binding.ClaimName("https://sentry.example.test", "company", "checkout-api"),
		Pending:        &binding.OperationState{Kind: "CreateProject", Phase: "Submitted", DesiredHash: testDigest("desired")},
		PinnedAdapters: map[string]string{"project": "legacy-24.7.1", "alerts": "legacy-issue-24.7.1"},
		Alerts: map[string]binding.AlertBinding{
			"new-issue": {
				Owner:   binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: project.Name, UID: project.UID, Generation: project.Generation},
				Adapter: "legacy-issue-24.7.1", ExternalIdentity: "external-alert-id",
				Trigger: "first-seen", DesiredHash: testDigest("alert"), Enabled: true,
				Destinations: []string{"telegram/ops"}, Marker: *marker,
			},
		},
		Destinations: map[string]binding.DestinationBinding{
			"telegram/ops": {
				Owner: binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: project.Name, UID: project.UID, Generation: project.Generation},
				Kind:  "telegram", ResourceName: "ops", State: "Active", SelectorHash: testDigest("selector"),
				Telegram: &binding.TelegramBinding{
					Target: &binding.TelegramTarget{ChatIdentity: "-100123"}, Phase: registration.PhasePendingPublish,
					OutboxKey: testDigest("outbox"), RouteKey: testDigest("route"), FenceHash: testDigest("fence"),
				},
			},
		},
	}
	if _, err := bindingStore.CreateBinding(ctx, projectBinding); err != nil {
		t.Fatal(err)
	}
	claim := binding.ManagerClaim{
		Version: binding.CurrentVersion, Installation: projectBinding.Installation, Organization: projectBinding.Organization,
		Slug: projectBinding.Slug, Owner: projectBinding.Owner, Manager: project, ExternalIdentity: projectBinding.ExternalIdentity,
	}
	if _, created, err := bindingStore.AcquireClaim(ctx, claim); err != nil || !created {
		t.Fatalf("claim = (created=%t, error=%v)", created, err)
	}
	plannedOperation := sentryapi.Operation{
		Key: "create-project-operation", Kind: "CreateProject", LocalResource: "checkout/api",
		IdempotencyHash: testDigest("operation"), Phase: sentryapi.OperationPlanned, Revision: 1,
	}
	if err := bindingStore.CompareAndSwap(ctx, 0, plannedOperation); err != nil {
		t.Fatal(err)
	}
	operation := plannedOperation
	operation.Phase = sentryapi.OperationSubmitted
	operation.Revision = 2
	if err := bindingStore.CompareAndSwap(ctx, plannedOperation.Revision, operation); err != nil {
		t.Fatal(err)
	}
	challenge := registration.ChallengeIndex{
		Version: binding.CurrentVersion, CodeHash: testDigest("challenge"), ProjectUID: project.UID,
		Destination: "telegram", ExpiresAt: now.Add(time.Hour),
	}
	if err := (&registration.ChallengeIndexStore{Client: kubernetesClient, Namespace: namespaces.State}).Create(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	routeStore := &registration.RouteStore{Client: kubernetesClient, Namespace: namespaces.Routing}
	routeTarget := binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "7"}
	if err := routeStore.PublishTelegramRoute(ctx, testDigest("route"), routeTarget); err != nil {
		t.Fatal(err)
	}
	sourceStore := &sentryhook.SourceStore{Client: kubernetesClient, Namespace: namespaces.HookSources}
	source := sentryhook.SourceRecord{
		Version: "v1alpha1", Revision: 1, SourceBindingKey: testDigest("source"), ExpectedProjectSlug: projectBinding.Slug,
		Deliveries: []sentryhook.TelegramDelivery{{
			RouteKey: testDigest("route"), Render: sentryhook.RenderInput{
				ProjectName: project.Name, AlertName: "new-issue", Environment: "production", Severity: sentryhook.SeverityCritical,
			},
		}},
	}
	if _, err := sourceStore.Create(ctx, source); err != nil {
		t.Fatal(err)
	}

	queue := &delivery.Queue{
		Client: kubernetesClient, Namespace: namespaces.Delivery, MaxOutstanding: 10, MaxAttempts: 2,
		DedupeRetention: time.Hour, DLQRetention: 24 * time.Hour, Now: func() time.Time { return now },
	}
	owner := testDigest("worker")
	for index, state := range []delivery.State{delivery.StateInFlight, delivery.StateRetry, delivery.StateDeadLetter} {
		message := delivery.Message{DedupeKey: testDigest("message-" + string(rune('a'+index))), RouteKey: testDigest("route"), Text: "protected payload"}
		if _, _, err := queue.Enqueue(ctx, message); err != nil {
			t.Fatal(err)
		}
		claimed, err := queue.Claim(ctx, owner, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		switch state {
		case delivery.StateRetry:
			if _, err := queue.Fail(ctx, claimed, delivery.FailureUnavailable, time.Minute, false); err != nil {
				t.Fatal(err)
			}
		case delivery.StateDeadLetter:
			if _, err := queue.Fail(ctx, claimed, delivery.FailureRejected, 0, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	admissionKey := testDigest("admission")
	admissionPayload, err := json.Marshal(delivery.Record{
		Version: "v1alpha1", Revision: 1, DedupeKey: admissionKey, MessageHash: testDigest("admission-message"),
		RouteKey: testDigest("route"), Text: "protected admission payload", State: delivery.StatePending,
		AdmissionPending: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	immutable := true
	admissionSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespaces.Delivery, Name: "sentry-telegram-admission-" + admissionKey,
			Labels: map[string]string{recordTypeLabel: "telegram-delivery-admission"},
		},
		Immutable: &immutable, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"admission.json": admissionPayload},
	}
	if err := kubernetesClient.Create(ctx, admissionSecret); err != nil {
		t.Fatal(err)
	}

	backupStore := &Store{Client: kubernetesClient, Namespaces: namespaces, Now: func() time.Time { return now }}
	bundle, err := backupStore.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Records) != 10 || !validDigest(bundle.Digest) {
		t.Fatalf("bundle records=%d digest=%q", len(bundle.Records), bundle.Digest)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Bundle
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	deleteAllSecrets(t, ctx, kubernetesClient, namespaces)

	restored, err := backupStore.Restore(ctx, decoded)
	if err != nil || restored.Created != len(bundle.Records) || restored.Existing != 0 {
		t.Fatalf("restore = (%+v, %v)", restored, err)
	}
	resumed, err := backupStore.Restore(ctx, decoded)
	if err != nil || resumed.Created != 0 || resumed.Existing != len(bundle.Records) {
		t.Fatalf("resumed restore = (%+v, %v)", resumed, err)
	}
	second, err := backupStore.Export(ctx)
	if err != nil || second.Digest != bundle.Digest {
		t.Fatalf("second export digest=%q error=%v, want %q", second.Digest, err, bundle.Digest)
	}

	restoredBinding, err := bindingStore.GetBinding(ctx, project.UID)
	if err != nil {
		t.Fatalf("restored binding: %v", err)
	}
	if !reflect.DeepEqual(restoredBinding.Binding.Alerts, projectBinding.Alerts) || restoredBinding.Binding.PinnedAdapters["alerts"] != "legacy-issue-24.7.1" {
		t.Fatalf("restored alert bindings = %#v, want %#v", restoredBinding.Binding.Alerts, projectBinding.Alerts)
	}
	if _, err := bindingStore.GetClaim(ctx, claim.Installation, claim.Organization, claim.Slug); err != nil {
		t.Fatalf("restored claim: %v", err)
	}
	if restoredOperation, found, err := bindingStore.Load(ctx, operation.Key); err != nil || !found || restoredOperation.Revision != operation.Revision {
		t.Fatalf("restored operation = (%+v, %t, %v)", restoredOperation, found, err)
	}
	if target, err := routeStore.GetTelegramRoute(ctx, testDigest("route")); err != nil || target != routeTarget {
		t.Fatalf("restored route = (%+v, %v)", target, err)
	}
	if deliveries, err := sourceStore.ResolveTelegramDeliveries(ctx, source.SourceBindingKey); err != nil || len(deliveries) != 1 || deliveries[0].RouteKey != testDigest("route") {
		t.Fatalf("restored hook source = (%+v, %v)", deliveries, err)
	}
	statistics, err := queue.Statistics(ctx)
	if err != nil || statistics.ByState[delivery.StateInFlight] != 1 || statistics.ByState[delivery.StateRetry] != 1 || statistics.ByState[delivery.StateDeadLetter] != 1 || statistics.ByState[delivery.StatePending] != 1 {
		t.Fatalf("restored queue = (%+v, %v)", statistics, err)
	}

	tampered := decoded
	tampered.Records = append([]Record(nil), decoded.Records...)
	tampered.Records[0].Data = cloneData(tampered.Records[0].Data)
	for key := range tampered.Records[0].Data {
		tampered.Records[0].Data[key][0] ^= 0xff
		break
	}
	if _, err := backupStore.Restore(ctx, tampered); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered restore error = %v, want ErrInvalid", err)
	}

	first := decoded.Records[0]
	secret := &corev1.Secret{}
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: first.Namespace, Name: first.Name}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.Immutable != nil && *secret.Immutable {
		for _, candidate := range decoded.Records {
			if !candidate.Immutable {
				first = candidate
				break
			}
		}
		if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: first.Namespace, Name: first.Name}, secret); err != nil {
			t.Fatal(err)
		}
	}
	secret.Labels["sentryconfiguration.io/delivery-state"] = "conflict"
	if err := kubernetesClient.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := backupStore.Restore(ctx, decoded); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting restore error = %v, want ErrConflict", err)
	}
}

func deleteAllSecrets(t *testing.T, ctx context.Context, kubernetesClient client.Client, namespaces Namespaces) {
	t.Helper()
	for _, namespace := range protectedNamespaces(namespaces) {
		secrets := &corev1.SecretList{}
		if err := kubernetesClient.List(ctx, secrets, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
		for index := range secrets.Items {
			if err := kubernetesClient.Delete(ctx, &secrets.Items[index]); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
		}
	}
}

func testDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
