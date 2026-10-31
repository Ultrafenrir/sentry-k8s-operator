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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

func TestExportFailsClosedOnUnknownProtectedNamespaceSecret(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	unknown := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: "unclassified"},
		Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"value": []byte("must-not-be-silently-omitted")},
	}
	store := &Store{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(unknown).Build(),
		Namespaces: Namespaces{State: "state", Routing: "routing", Delivery: "delivery", HookSources: "hook-sources"},
	}
	if _, err := store.Export(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("export error = %v, want ErrInvalid", err)
	}
}

func TestExportFailsClosedOnSemanticallyInvalidStateRecord(t *testing.T) {
	t.Parallel()

	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "operator"}
	corruptBinding := binding.ProjectBinding{
		Version:      binding.CurrentVersion,
		Project:      binding.ProjectReference{Namespace: "payments", Name: "checkout", UID: "uid", Generation: 1},
		Installation: "https://sentry.example", Organization: "company", Slug: "checkout",
		Owner: "payments", Origin: binding.OriginManaged, AccessMode: binding.AccessManage,
		Marker: &marker, ManagerUID: "uid", ClaimName: "claim",
		PinnedAdapters: map[string]string{"alerts": "legacy"},
		Alerts: map[string]binding.AlertBinding{
			"rule": {
				Adapter: "different-adapter", ExternalIdentity: "alert", Trigger: "first-seen",
				DesiredHash: strings.Repeat("a", 64), Enabled: true, Destinations: []string{"telegram/chat"}, Marker: marker,
			},
		},
	}
	corruptOperation := sentryapi.Operation{
		Key: "operation", Kind: "UpdateAlert", LocalResource: "payments/checkout",
		IdempotencyHash: strings.Repeat("b", 64), Phase: sentryapi.OperationPhase("Unknown"), Revision: 1,
	}
	for name, value := range map[string]struct {
		recordType string
		payload    any
	}{
		"binding":   {recordType: "project-binding", payload: corruptBinding},
		"operation": {recordType: "operation-journal", payload: corruptOperation},
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(value.payload)
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: name, Labels: map[string]string{recordTypeLabel: value.recordType}},
				Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"state.json": payload},
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			store := &Store{
				Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
				Namespaces: Namespaces{State: "state", Routing: "routing", Delivery: "delivery", HookSources: "hook-sources"},
			}
			if _, err := store.Export(context.Background()); !errors.Is(err, ErrInvalid) {
				t.Fatalf("export error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRestoreRejectsChecksumValidSemanticCorruptionBeforeWrite(t *testing.T) {
	t.Parallel()

	namespaces := Namespaces{State: "state", Routing: "routing", Delivery: "delivery", HookSources: "hook-sources"}
	operation := sentryapi.Operation{
		Key: "operation", Kind: "UpdateAlert", LocalResource: "payments/checkout",
		IdempotencyHash: strings.Repeat("b", 64), Phase: sentryapi.OperationPhase("Unknown"), Revision: 1,
	}
	payload, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		Namespace: namespaces.State, Name: "corrupt-operation", RecordType: "operation-journal",
		Labels: map[string]string{recordTypeLabel: "operation-journal"},
		Data:   map[string][]byte{"state.json": payload},
	}
	record.Digest, err = recordDigest(record)
	if err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{
		Version: CurrentVersion, CreatedAt: time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC),
		Namespaces: namespaces, Records: []Record{record},
	}
	bundle.Digest, err = bundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := &Store{Client: client, Namespaces: namespaces}
	if _, err := store.Restore(context.Background(), bundle); !errors.Is(err, ErrInvalid) {
		t.Fatalf("restore error = %v, want ErrInvalid", err)
	}
	secrets := &corev1.SecretList{}
	if err := client.List(context.Background(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("semantic preflight wrote records: count=%d err=%v", len(secrets.Items), err)
	}
}

func TestExportFailsClosedOnInvalidChallengeRouteHookSourceAndDeliveryPayloads(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		namespace  string
		name       string
		recordType string
		dataKey    string
		immutable  bool
		stateLabel string
		payload    any
	}{
		"challenge": {
			namespace: "state", name: "sentry-telegram-challenge-" + digest,
			recordType: "telegram-challenge-index", dataKey: "challenge.json", immutable: true,
			payload: map[string]any{"version": binding.CurrentVersion, "codeHash": digest, "projectUID": "", "destination": "chat", "expiresAt": now.Add(time.Hour)},
		},
		"route": {
			namespace: "routing", name: "sentry-telegram-route-" + digest,
			recordType: "telegram-route", dataKey: "route.json", immutable: true,
			payload: map[string]any{"version": binding.CurrentVersion, "outboxKey": digest, "target": map[string]any{"chatIdentity": "not-numeric"}},
		},
		"delivery": {
			namespace: "delivery", name: "sentry-telegram-delivery-" + digest,
			recordType: "telegram-delivery", dataKey: "delivery.json", stateLabel: string(delivery.StatePending),
			payload: delivery.Record{
				Version: "v1alpha1", Revision: 1, DedupeKey: digest, MessageHash: digest, RouteKey: digest,
				State: delivery.StatePending, CreatedAt: now, UpdatedAt: now,
			},
		},
		"hook-source": {
			namespace: "hook-sources", name: "sentry-hook-source-" + digest,
			recordType: "sentry-hook-source", dataKey: "source.json",
			payload: sentryhook.SourceRecord{
				Version: "v1alpha1", Revision: 1, SourceBindingKey: digest, ExpectedProjectSlug: "checkout-api",
				Deliveries: []sentryhook.TelegramDelivery{{RouteKey: "invalid", Render: sentryhook.RenderInput{ProjectName: "checkout", AlertName: "rule", Severity: sentryhook.SeverityWarning}}},
			},
		},
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(value.payload)
			if err != nil {
				t.Fatal(err)
			}
			labels := map[string]string{recordTypeLabel: value.recordType}
			if value.stateLabel != "" {
				labels[recordStateLabel] = value.stateLabel
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: value.namespace, Name: value.name, Labels: labels},
				Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{value.dataKey: payload},
			}
			if value.immutable {
				immutable := true
				secret.Immutable = &immutable
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			store := &Store{
				Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
				Namespaces: Namespaces{State: "state", Routing: "routing", Delivery: "delivery", HookSources: "hook-sources"},
			}
			if _, err := store.Export(context.Background()); !errors.Is(err, ErrInvalid) {
				t.Fatalf("export error = %v, want ErrInvalid", err)
			}
		})
	}
}
