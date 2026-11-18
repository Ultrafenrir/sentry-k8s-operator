package validation

import (
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestCronMonitorValidation(t *testing.T) {
	valid := sentryv1alpha1.SentryCronMonitorSpec{ProjectRef: "checkout", Crontab: &sentryv1alpha1.CronSchedule{Expression: "*/5 * * * *", Timezone: "Europe/Nicosia"}}
	if err := CronMonitor(valid); err != nil {
		t.Fatalf("valid Cron Monitor: %v", err)
	}
	valid.Interval = &sentryv1alpha1.IntervalSchedule{Value: 5, Unit: "minute"}
	if err := CronMonitor(valid); err == nil {
		t.Fatal("two schedule variants must fail")
	}
	valid.Crontab = nil
	valid.Interval = nil
	if err := CronMonitor(valid); err == nil {
		t.Fatal("missing schedule must fail")
	}
}

func TestCronMonitorValidatesPrimitiveBoundsInTheController(t *testing.T) {
	margin := int32(10081)
	spec := sentryv1alpha1.SentryCronMonitorSpec{ProjectRef: "checkout", Interval: &sentryv1alpha1.IntervalSchedule{Value: 1, Unit: "hour"}, CheckInMarginMinutes: &margin}
	if err := CronMonitor(spec); err == nil {
		t.Fatal("out-of-range check-in margin must fail without relying on CRD admission")
	}
	spec.CheckInMarginMinutes = nil
	spec.OwnerTeam = "Invalid_Team"
	if err := CronMonitor(spec); err == nil {
		t.Fatal("invalid owner team must fail without relying on CRD admission")
	}
}

func TestUptimeMonitorRejectsSecretExfiltrationAndUnsafeDestinations(t *testing.T) {
	base := sentryv1alpha1.SentryUptimeMonitorSpec{ProjectRef: "checkout", URL: "https://example.com/health", Method: "POST", IntervalSeconds: 300, TimeoutMilliseconds: 5000, Headers: []sentryv1alpha1.UptimeHeader{{Name: "X-Probe", ValueFrom: sentryv1alpha1.SecretValueReference{Name: "sentry-uptime-inputs", Key: "probe"}}}}
	if err := UptimeMonitor(base); err != nil {
		t.Fatalf("valid Uptime Monitor: %v", err)
	}
	unsafe := base
	unsafe.URL = "http://127.0.0.1:8080/secret"
	if err := UptimeMonitor(unsafe); err == nil {
		t.Fatal("loopback destination must fail")
	}
	unsafe = base
	unsafe.Headers[0].Name = "Authorization"
	if err := UptimeMonitor(unsafe); err == nil {
		t.Fatal("credential-bearing header must fail")
	}
	unsafe = base
	unsafe.Headers[0].ValueFrom.Name = "arbitrary-secret"
	if err := UptimeMonitor(unsafe); err == nil {
		t.Fatal("arbitrary Secret reference must fail")
	}
}

func TestUptimeMonitorValidatesPrimitiveBoundsInTheController(t *testing.T) {
	spec := sentryv1alpha1.SentryUptimeMonitorSpec{ProjectRef: "checkout", URL: "https://example.com/health", Method: "GET", IntervalSeconds: 300, TimeoutMilliseconds: 5000, FailureThreshold: 1001}
	if err := UptimeMonitor(spec); err == nil {
		t.Fatal("out-of-range failure threshold must fail without relying on CRD admission")
	}
	spec.FailureThreshold = 1
	spec.Assertions = []sentryv1alpha1.UptimeAssertion{{Type: "statusCode", Comparison: "equals"}}
	if err := UptimeMonitor(spec); err == nil {
		t.Fatal("empty assertion value must fail without relying on CRD admission")
	}
}

func TestOrderedPolicyReferencesAreUniqueAndExclusive(t *testing.T) {
	base := sentryv1alpha1.SentryProjectSpec{Sentry: "company", Team: "payments"}
	base.AlertPolicy = "legacy"
	base.AlertPolicies = []string{"first"}
	if err := Project(base); err == nil {
		t.Fatal("legacy and ordered references must be exclusive")
	}
	base.AlertPolicy = ""
	base.AlertPolicies = []string{"same", "same"}
	if err := Project(base); err == nil {
		t.Fatal("duplicate ordered references must fail")
	}
}
