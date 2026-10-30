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
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSourceStoreCreateResolveCASAndExactDelete(t *testing.T) {
	t.Parallel()
	store := testSourceStore(t)
	record := validSourceRecord()
	created, err := store.Create(context.Background(), record)
	if err != nil || created.ResourceVersion == "" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	resolved, err := store.ResolveTelegramDeliveries(context.Background(), record.SourceBindingKey)
	if err != nil || len(resolved) != 2 || resolved[0].RouteKey >= resolved[1].RouteKey {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	resolved[0].Render.ProjectName = "caller-mutation"
	again, err := store.ResolveTelegramDeliveries(context.Background(), record.SourceBindingKey)
	if err != nil || again[0].Render.ProjectName == "caller-mutation" {
		t.Fatalf("resolver returned aliased state: %#v err=%v", again, err)
	}
	next := created
	next.Revision++
	next.Deliveries = next.Deliveries[:1]
	updated, err := store.CompareAndSwap(context.Background(), created, next)
	if err != nil || updated.Revision != 2 || updated.ResourceVersion == created.ResourceVersion {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if err := store.DeleteExact(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), record.SourceBindingKey); err == nil {
		t.Fatal("deleted source record remained readable")
	}
}

func TestSourceStoreMetadataAndPayloadAreClosed(t *testing.T) {
	t.Parallel()
	store := testSourceStore(t)
	record := validSourceRecord()
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	secrets := &corev1.SecretList{}
	if err := store.Client.List(context.Background(), secrets, client.InNamespace(store.Namespace)); err != nil || len(secrets.Items) != 1 {
		t.Fatalf("secrets=%d err=%v", len(secrets.Items), err)
	}
	metadata, err := json.Marshal(struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}{secrets.Items[0].Name, secrets.Items[0].Labels})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"checkout", "new-issue", "production", strings.Repeat("b", 64)} {
		if strings.Contains(string(metadata), forbidden) {
			t.Fatalf("metadata contains protected value %q: %s", forbidden, metadata)
		}
	}
	if err := ValidateSourceRecordPayload(secrets.Items[0].Name, secrets.Items[0].Data[sourceDataKey]); err != nil {
		t.Fatal(err)
	}
}

func TestSourceStoreRejectsNonCanonicalOrMutableIdentityBeforeWrite(t *testing.T) {
	t.Parallel()
	store := testSourceStore(t)
	for _, mutate := range []func(*SourceRecord){
		func(record *SourceRecord) { record.SourceBindingKey = "not-a-hash" },
		func(record *SourceRecord) { record.ExpectedProjectSlug = "External ID" },
		func(record *SourceRecord) { record.Deliveries[0].RouteKey = record.Deliveries[1].RouteKey },
		func(record *SourceRecord) { record.Deliveries[0].Render.ProjectName = "unsafe\nname" },
		func(record *SourceRecord) { record.Revision = 2 },
	} {
		record := validSourceRecord()
		mutate(&record)
		if _, err := store.Create(context.Background(), record); !errors.Is(err, ErrSourceRecord) {
			t.Fatalf("record=%#v error=%v", record, err)
		}
	}
	secrets := &corev1.SecretList{}
	if err := store.Client.List(context.Background(), secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("invalid source records created state: count=%d err=%v", len(secrets.Items), err)
	}
	created, err := store.Create(context.Background(), validSourceRecord())
	if err != nil {
		t.Fatal(err)
	}
	next := created
	next.Revision++
	next.ExpectedProjectSlug = "different-slug"
	if _, err := store.CompareAndSwap(context.Background(), created, next); !errors.Is(err, ErrSourceRecord) {
		t.Fatalf("identity-changing CAS error=%v", err)
	}
}

func TestSourceStoreRejectsRecordWithoutExactManagedByLabel(t *testing.T) {
	t.Parallel()
	store := testSourceStore(t)
	record := validSourceRecord()
	if _, err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := store.Client.Get(context.Background(), client.ObjectKey{Namespace: store.Namespace, Name: sourceRecordName(record.SourceBindingKey)}, secret); err != nil {
		t.Fatal(err)
	}
	delete(secret.Labels, "app.kubernetes.io/managed-by")
	if err := store.Client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveTelegramDeliveries(context.Background(), record.SourceBindingKey); !errors.Is(err, ErrSourceRecord) {
		t.Fatalf("resolve error=%v", err)
	}
}

func testSourceStore(t *testing.T) *SourceStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &SourceStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state"}
}

func validSourceRecord() SourceRecord {
	return SourceRecord{
		Version: sourceRecordVersion, Revision: 1, SourceBindingKey: strings.Repeat("a", 64), ExpectedProjectSlug: "checkout-api",
		Deliveries: []TelegramDelivery{
			{RouteKey: strings.Repeat("b", 64), Render: RenderInput{ProjectName: "checkout", AlertName: "new-issue", Environment: "production", Severity: SeverityWarning}},
			{RouteKey: strings.Repeat("c", 64), Render: RenderInput{ProjectName: "checkout", AlertName: "regression", Severity: SeverityCritical}},
		},
	}
}
