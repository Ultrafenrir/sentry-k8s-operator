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

package audit

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesStoreFailsClosedBeforeCreateAndOnCorruption(t *testing.T) {
	t.Parallel()
	store := auditStore(t)
	store.Random = bytes.NewReader(bytes.Repeat([]byte{1}, 8))
	if err := store.Record(context.Background(), Event{Component: ComponentAlerts}); !errors.Is(err, ErrStore) {
		t.Fatalf("invalid event error=%v", err)
	}
	secrets := &corev1.SecretList{}
	if err := store.Client.List(context.Background(), secrets, client.InNamespace(store.Namespace)); err != nil || len(secrets.Items) != 0 {
		t.Fatalf("invalid event created state: %#v err=%v", secrets.Items, err)
	}
	immutable := true
	if err := store.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: store.Namespace, Name: auditNamePrefix + "corrupt",
			Labels: map[string]string{RecordTypeLabel: RecordTypeAudit, "app.kubernetes.io/managed-by": "sentry-kubernetes-operator"},
		},
		Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{EventDataKey: []byte(`{"component":"alerts"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background()); !errors.Is(err, ErrStore) {
		t.Fatalf("corrupt list error=%v", err)
	}
	if deleted, err := store.SweepBefore(context.Background(), time.Now().Add(time.Hour), 10); deleted != 0 || !errors.Is(err, ErrStore) {
		t.Fatalf("corrupt sweep deleted=%d err=%v", deleted, err)
	}
}

func TestKubernetesStoreSweepIsBoundedAndUsesEventTime(t *testing.T) {
	t.Parallel()
	store := auditStore(t)
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for index, timestamp := range []time.Time{base.Add(-3 * time.Hour), base.Add(-2 * time.Hour), base.Add(-time.Hour), base} {
		store.Now = func() time.Time { return timestamp }
		store.Random = bytes.NewReader(bytes.Repeat([]byte{byte(index + 1)}, 8))
		if err := store.Record(context.Background(), Event{
			Component: ComponentProjects, Action: ActionDelete, Outcome: OutcomeSucceeded,
			Reason: ReasonMutationVerified, LocalResource: "payments/checkout", Revision: int64(index + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.SweepBefore(context.Background(), base.Add(-30*time.Minute), 2)
	if err != nil || deleted != 2 {
		t.Fatalf("first sweep deleted=%d err=%v", deleted, err)
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != 2 || !records[0].Timestamp.Equal(base.Add(-time.Hour)) || !records[1].Timestamp.Equal(base) {
		t.Fatalf("remaining=%#v err=%v", records, err)
	}
	deleted, err = store.SweepBefore(context.Background(), base.Add(-30*time.Minute), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("second sweep deleted=%d err=%v", deleted, err)
	}
}

func auditStore(t *testing.T) *KubernetesStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &KubernetesStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "audit"}
}
