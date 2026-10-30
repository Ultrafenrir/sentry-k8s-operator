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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
)

func TestExportAuditEmitsOnlyValidatedEventsInOrder(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := &audit.KubernetesStore{Client: kubernetesClient, Namespace: "audit"}
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for index, timestamp := range []time.Time{base.Add(time.Minute), base} {
		store.Now = func() time.Time { return timestamp }
		store.Random = bytes.NewReader(bytes.Repeat([]byte{byte(index + 1)}, 8))
		if err := store.Record(context.Background(), audit.Event{
			Component: audit.ComponentProjects, Action: audit.ActionDelete, Outcome: audit.OutcomeSucceeded,
			Reason: audit.ReasonMutationVerified, LocalResource: "payments/checkout", Revision: int64(index + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := exportAudit(context.Background(), kubernetesClient, "audit", &output); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"sentry-audit-", "resourceVersion", "externalIdentity", "payload", "rawError"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("export contains %q: %s", forbidden, output.String())
		}
	}
	var result struct {
		Events []audit.Event `json:"events"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 || !result.Events[0].Timestamp.Equal(base) || !result.Events[1].Timestamp.Equal(base.Add(time.Minute)) {
		t.Fatalf("events=%#v", result.Events)
	}
}

func TestExportAuditFailsClosedWithoutPartialOutput(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := &audit.KubernetesStore{Client: kubernetesClient, Namespace: "audit", Now: func() time.Time {
		return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	}, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 8))}
	if err := store.Record(context.Background(), audit.Event{
		Component: audit.ComponentProjects, Action: audit.ActionDelete, Outcome: audit.OutcomeSucceeded,
		Reason: audit.ReasonMutationVerified, LocalResource: "payments/checkout", Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	secrets := &corev1.SecretList{}
	if err := kubernetesClient.List(context.Background(), secrets); err != nil || len(secrets.Items) != 1 {
		t.Fatalf("secrets=%d err=%v", len(secrets.Items), err)
	}
	secrets.Items[0].Data[audit.EventDataKey] = []byte(`{"component":"projects"}`)
	if err := kubernetesClient.Update(context.Background(), &secrets.Items[0]); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := exportAudit(context.Background(), kubernetesClient, "audit", &output); err == nil || output.Len() != 0 {
		t.Fatalf("error=%v output=%q", err, output.String())
	}
}

func TestAuditExportRejectsInvalidFlagsBeforeKubeconfig(t *testing.T) {
	t.Parallel()
	if err := run([]string{"audit-export", "--timeout=0s"}, &bytes.Buffer{}); err == nil || err.Error() != "positive timeout and --audit-namespace are required" {
		t.Fatalf("error=%v", err)
	}
}
