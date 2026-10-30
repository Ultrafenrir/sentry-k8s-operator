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

package adminreport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

func TestReportSummarizesRecoveryStateWithoutProtectedValues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	const externalSentinel = "external-id-SHOULD-NOT-LEAK"
	const chatSentinel = "-100987654321"
	const payloadSentinel = "private-event-payload-SHOULD-NOT-LEAK"
	const operationKeySentinel = "operation-key-SHOULD-NOT-LEAK"
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "operator"}

	bindingValue := binding.ProjectBinding{
		Version: binding.CurrentVersion,
		Project: binding.ProjectReference{Namespace: "checkout", Name: "api", UID: "project-uid", Generation: 3},
		Origin:  binding.OriginManaged, AccessMode: binding.AccessManage, Owner: marker.Owner, Marker: &marker, ExternalIdentity: externalSentinel,
		Pending:        &binding.OperationState{Kind: externalSentinel, Phase: externalSentinel},
		PinnedAdapters: map[string]string{"alerts": "legacy-issue"},
		Alerts: map[string]binding.AlertBinding{
			"new-issue": {
				Adapter: "legacy-issue", ExternalIdentity: externalSentinel, Trigger: externalSentinel,
				DesiredHash: strings.Repeat("a", 64), Enabled: true, Destinations: []string{"ops"}, Marker: marker,
			},
		},
		Tombstones: map[string]bool{operationKeySentinel: true},
		Destinations: map[string]binding.DestinationBinding{
			"ops": {
				Kind: "telegram", State: "Active", Reason: externalSentinel,
				Telegram: &binding.TelegramBinding{
					Target: &binding.TelegramTarget{ChatIdentity: chatSentinel}, RouteKey: strings.Repeat("a", 64),
				},
			},
		},
	}
	claimValue := binding.ManagerClaim{
		Version: binding.CurrentVersion, Installation: "company", Organization: "org", Slug: "checkout-api",
		Manager: binding.ProjectReference{Namespace: "checkout", Name: "api"}, ExternalIdentity: externalSentinel,
	}
	operationValue := sentryapi.Operation{
		Key: operationKeySentinel, Kind: "CreateProject", LocalResource: "checkout/api",
		IdempotencyHash: strings.Repeat("b", 64), Phase: sentryapi.OperationSubmitted, Revision: 2,
	}
	immutable := true
	challengeSecret := secretWithJSON(t, "state", "challenge", "telegram-challenge-index", "challenge.json", map[string]any{
		"codeHash": externalSentinel, "projectUID": externalSentinel, "expiresAt": now.Add(-time.Minute),
	})
	challengeSecret.Immutable = &immutable
	activeAdmissionKey := strings.Repeat("e", 64)
	expiredAdmissionKey := strings.Repeat("f", 64)
	activeAdmission := secretWithJSON(t, "delivery", "sentry-telegram-admission-"+activeAdmissionKey, "telegram-delivery-admission", "admission.json", delivery.Record{
		Version: "v1alpha1", Revision: 1, DedupeKey: activeAdmissionKey, MessageHash: strings.Repeat("a", 64),
		RouteKey: strings.Repeat("b", 64), Text: payloadSentinel, State: delivery.StatePending,
		AdmissionPending: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute),
	})
	activeAdmission.Immutable = &immutable
	expiredAdmission := secretWithJSON(t, "delivery", "sentry-telegram-admission-"+expiredAdmissionKey, "telegram-delivery-admission", "admission.json", delivery.Record{
		Version: "v1alpha1", Revision: 1, DedupeKey: expiredAdmissionKey, MessageHash: strings.Repeat("a", 64),
		RouteKey: strings.Repeat("b", 64), Text: payloadSentinel, State: delivery.StatePending,
		AdmissionPending: true, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	})
	expiredAdmission.Immutable = &immutable
	objects := []client.Object{
		secretWithJSON(t, "state", "binding", "project-binding", "state.json", bindingValue),
		secretWithJSON(t, "state", "claim", "manager-claim", "state.json", claimValue),
		secretWithJSON(t, "state", "operation", "operation-journal", "state.json", operationValue),
		secretWithJSON(t, "state", "invalid-operation", "operation-journal", "state.json", sentryapi.Operation{
			Key: "invalid", Kind: "UpdateAlert", LocalResource: "checkout/rule",
			IdempotencyHash: "not-a-hash", Phase: sentryapi.OperationSubmitted, Revision: 2,
		}),
		challengeSecret,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "routing", Name: "route", Labels: map[string]string{recordTypeLabel: "telegram-route"}},
			Immutable:  &immutable, Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"route.json": []byte(`{"target":{"chatIdentity":"` + chatSentinel + `"}}`)},
		},
		secretWithJSON(t, "delivery", "delivery", "telegram-delivery", "delivery.json", map[string]any{
			"state": "Retry", "attempts": 2, "text": payloadSentinel, "routeKey": externalSentinel,
		}),
		activeAdmission,
		expiredAdmission,
		secretWithJSON(t, "hook-sources", "sentry-hook-source-"+strings.Repeat("c", 64), "sentry-hook-source", "source.json", sentryhook.SourceRecord{
			Version: "v1alpha1", Revision: 1, SourceBindingKey: strings.Repeat("c", 64), ExpectedProjectSlug: "checkout-api",
			Deliveries: []sentryhook.TelegramDelivery{{RouteKey: strings.Repeat("d", 64), Render: sentryhook.RenderInput{
				ProjectName: payloadSentinel, AlertName: "new-issue", Severity: sentryhook.SeverityCritical,
			}}},
		}),
		secretWithJSON(t, "hook-sources", "invalid-source", "sentry-hook-source", "source.json", sentryhook.SourceRecord{
			Version: "v1alpha1", Revision: 1, SourceBindingKey: "not-a-hash", ExpectedProjectSlug: "checkout-api",
		}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "routing", Name: "invalid", Labels: map[string]string{recordTypeLabel: "telegram-route"}},
			Type:       corev1.SecretTypeOpaque,
		},
	}
	reporter := testReporter(t, now, objects...)
	report, err := reporter.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	for _, forbidden := range []string{externalSentinel, chatSentinel, payloadSentinel, operationKeySentinel} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("report leaked %q: %s", forbidden, output)
		}
	}
	if len(report.Bindings) != 1 || report.Bindings[0].Namespace != "checkout" || !report.Bindings[0].Pending || len(report.Bindings[0].Destinations) != 1 || !report.Bindings[0].Destinations[0].TargetPinned || !report.Bindings[0].Destinations[0].RoutePresent {
		t.Fatalf("binding summary = %+v", report.Bindings)
	}
	if report.Bindings[0].Alerts.Total != 1 || report.Bindings[0].Alerts.Enabled != 1 || report.Bindings[0].Alerts.Disabled != 0 || report.Bindings[0].Alerts.Tombstones != 1 {
		t.Fatalf("alert summary = %+v", report.Bindings[0].Alerts)
	}
	if len(report.Claims) != 1 || len(report.Operations) != 1 || report.Challenges.Total != 1 || report.Challenges.Expired != 1 || report.Routes.Total != 1 || report.Invalid["telegram-route"] != 1 || report.Invalid["operation-journal"] != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Queue.Total != 1 || report.Queue.ByState["Retry"] != 1 || report.Queue.MaxAttempts != 2 || report.Queue.AdmissionPending != 2 || report.Queue.ExpiredAdmission != 1 {
		t.Fatalf("queue = %+v", report.Queue)
	}
	if report.HookSources.Total != 1 || report.Invalid["sentry-hook-source"] != 1 {
		t.Fatalf("hook sources = %+v invalid=%+v", report.HookSources, report.Invalid)
	}
}

func TestReportFailsClosedOnNamespaceReadError(t *testing.T) {
	t.Parallel()
	reporter := testReporter(t, time.Now())
	reporter.StateNamespace = ""
	if _, err := reporter.Build(context.Background()); err != ErrReport {
		t.Fatalf("error = %v, want ErrReport", err)
	}
}

func secretWithJSON(t *testing.T, namespace, name, recordType, key string, value any) *corev1.Secret {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{recordTypeLabel: recordType}
	if recordType == "sentry-hook-source" {
		labels["app.kubernetes.io/managed-by"] = "sentry-kubernetes-operator"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{key: payload},
	}
}

func testReporter(t *testing.T, now time.Time, objects ...client.Object) *Reporter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Reporter{
		Client:         fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		StateNamespace: "state", RoutingNamespace: "routing", DeliveryNamespace: "delivery",
		HookSourceNamespace: "hook-sources",
		Now:                 func() time.Time { return now },
	}
}
