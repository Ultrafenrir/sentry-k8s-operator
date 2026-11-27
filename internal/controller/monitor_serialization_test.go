package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/monitor"
)

func TestResolveMonitorProjectRequiresCurrentProjectReadyCondition(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("project-uid"), Generation: 2},
		Spec:       sentryv1alpha1.SentryProjectSpec{Sentry: "primary", Team: "payments"},
		Status: sentryv1alpha1.SentryProjectStatus{
			ObservedGeneration: 1,
			Conditions:         []metav1.Condition{{Type: "ProjectReady", Status: metav1.ConditionTrue, ObservedGeneration: 1}},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(project).Build()
	if _, err := resolveMonitorProject(context.Background(), kube, nil, nil, "payments", "checkout"); !errors.Is(err, errMonitorProjectNotReady) {
		t.Fatalf("stale project readiness error = %v", err)
	}
}

func TestSerializeAssertionsUsesStableUUIDIdentifiers(t *testing.T) {
	value := serializeAssertions([]sentryv1alpha1.UptimeAssertion{{Type: "statusCode", Comparison: "equals", Value: "200"}, {Type: "jsonPath", Key: "$.ready", Comparison: "equals", Value: "true"}, {Type: "responseHeader", Key: "Content-Type", Comparison: "equals", Value: "application/json"}})
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, expected := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004", "status_code_check", "json_path", "header_check", `"value":"$.ready"`, `"value":"true"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in %s", expected, text)
		}
	}
}

func TestUptimeMismatchIsStableAndDoesNotExposeValues(t *testing.T) {
	t.Parallel()
	body := "sensitive"
	request := monitor.UptimeRequest{Name: "managed", Status: "active", URL: "https://example.com/", IntervalSeconds: 300, TimeoutMilliseconds: 5000, Method: "POST", Headers: [][2]string{{"X-Input", "secret"}}, Body: &body, ResponseCaptureEnabled: true, RecoveryThreshold: 1, DowntimeThreshold: 3}
	remote := monitor.UptimeRemote{Identity: "20", Name: request.Name, Status: request.Status, URL: request.URL, IntervalSeconds: request.IntervalSeconds, TimeoutMilliseconds: request.TimeoutMilliseconds, Method: request.Method, Headers: append([][2]string(nil), request.Headers...), Body: request.Body, ResponseCaptureEnabled: request.ResponseCaptureEnabled, RecoveryThreshold: request.RecoveryThreshold, DowntimeThreshold: request.DowntimeThreshold}
	if mismatch := uptimeMismatch(remote, request); mismatch != "" {
		t.Fatalf("equal state mismatch = %q", mismatch)
	}
	remote.Headers[0][1] = "different-secret"
	mismatch := uptimeMismatch(remote, request)
	if mismatch != "headers" || strings.Contains(mismatch, "secret") {
		t.Fatalf("header mismatch = %q", mismatch)
	}
}

func TestCronMismatchIgnoresStaleTimezoneForIntervalSchedule(t *testing.T) {
	t.Parallel()
	margin, runtime, failure, recovery := int32(9), int32(31), int32(3), int32(2)
	request := monitor.CronRequest{Project: "checkout", Name: "cron", Slug: "managed", Status: "disabled", Config: monitor.CronConfig{ScheduleType: "interval", Schedule: []any{int32(2), "hour"}, CheckInMargin: &margin, MaxRuntime: &runtime, FailureThreshold: &failure, RecoveryThreshold: &recovery}}
	remote := monitor.CronRemote{Identity: "10", Name: request.Name, Slug: request.Slug, Status: request.Status, Config: request.Config}
	remote.Project.Slug = request.Project
	remote.Config.Timezone = "Europe/Nicosia"
	remote.Config.Schedule = []any{float64(2), "hour"}
	if mismatch := cronMismatch(remote, request); mismatch != "" {
		t.Fatalf("interval state mismatch = %q", mismatch)
	}
}

func TestMonitorDeletionPendingRequiresPinnedIdentity(t *testing.T) {
	t.Parallel()
	if !monitorDeletionPending("10", "10", "pending_deletion") {
		t.Fatal("matching Sentry deletion tombstone was not recognized")
	}
	for _, test := range []struct {
		identity string
		expected string
		status   string
	}{
		{identity: "11", expected: "10", status: "pending_deletion"},
		{identity: "10", expected: "10", status: "active"},
	} {
		if monitorDeletionPending(test.identity, test.expected, test.status) {
			t.Fatalf("unexpected pending deletion match for %#v", test)
		}
	}
}

func TestCronMonitorExternalURLMatchesCompatibilityProfile(t *testing.T) {
	t.Parallel()
	legacy := &sentryv1alpha1.ConnectionCompatibility{Alerts: sentryv1alpha1.AlertCompatibilityLegacy24}
	modern := &sentryv1alpha1.ConnectionCompatibility{Alerts: sentryv1alpha1.AlertCompatibilityModern26}
	if got := cronMonitorExternalURL("https://sentry.example/", "company", "checkout", "settlement", legacy); got != "https://sentry.example/organizations/company/crons/checkout/settlement/" {
		t.Fatalf("legacy URL = %q", got)
	}
	if got := cronMonitorExternalURL("https://sentry.example", "company", "checkout", "settlement", modern); got != "https://sentry.example/organizations/company/issues/alerts/rules/crons/checkout/settlement/details/" {
		t.Fatalf("modern URL = %q", got)
	}
}
