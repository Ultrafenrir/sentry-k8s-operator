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

package validation

import (
	"os"
	"path/filepath"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

func TestConnectionCrossFieldValidation(t *testing.T) {
	t.Parallel()
	valid := sentryv1alpha1.SentryConnectionSpec{URL: "https://sentry.example", ProjectManagement: sentryv1alpha1.ProjectManagement{UnmanagedProjects: sentryv1alpha1.UnmanagedProjectsReject}}
	if err := Connection(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]sentryv1alpha1.SentryConnectionSpec{
		"insecure HTTP":                  {URL: "http://sentry.example"},
		"skip TLS over HTTP":             {URL: "http://sentry.example", Transport: &sentryv1alpha1.ConnectionTransport{AllowInsecureHTTP: true, InsecureSkipTLSVerify: true}},
		"skip TLS with CA":               {URL: "https://sentry.example", Transport: &sentryv1alpha1.ConnectionTransport{InsecureSkipTLSVerify: true}, Credentials: sentryv1alpha1.ConnectionCredentials{CABundleSecretRef: &sentryv1alpha1.SecretKeyReference{Name: "ca", Key: "ca.crt"}}},
		"adopt without active ownership": {URL: "https://sentry.example", ProjectManagement: sentryv1alpha1.ProjectManagement{UnmanagedProjects: sentryv1alpha1.UnmanagedProjectsAdopt}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Connection(spec); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestProjectCrossFieldValidation(t *testing.T) {
	t.Parallel()
	base := func() sentryv1alpha1.SentryProjectSpec {
		return sentryv1alpha1.SentryProjectSpec{Team: "payments"}
	}
	if err := Project(base()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() sentryv1alpha1.SentryProjectSpec{
		"missing team": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.Team = ""
			return spec
		},
		"primary team grant": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.GrantAccess = &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"payments"}}
			return spec
		},
		"unattached ownership team": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.IssueOwnership = &sentryv1alpha1.IssueOwnershipSpec{Rules: []sentryv1alpha1.IssueOwnershipRule{{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"}, Teams: []string{"security"}}}}
			return spec
		},
		"multiple published managed keys": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.ClientKeys = &map[string]sentryv1alpha1.ManagedClientKeySpec{
				"one": {Active: true, PublishDSN: true},
				"two": {Active: true, PublishDSN: true},
			}
			return spec
		},
		"external and managed publication": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.ClientKeys = &map[string]sentryv1alpha1.ManagedClientKeySpec{"one": {Active: true, PublishDSN: true}}
			spec.ExistingClientKey = &sentryv1alpha1.ExistingClientKeySpec{Name: "Default", PublishDSN: true}
			return spec
		},
		"metric missing threshold": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			window := int32(5)
			spec.Alerts = &map[string]sentryv1alpha1.AlertSpec{"count": {Trigger: sentryv1alpha1.AlertTriggerEventCount, WindowMinutes: &window}}
			return spec
		},
		"symbol source variants": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.SymbolSources = &map[string]sentryv1alpha1.CustomSymbolSourceSpec{"symbols": {HTTP: &sentryv1alpha1.HTTPSymbolSource{URL: "https://symbols.example"}, S3: &sentryv1alpha1.S3SymbolSource{Bucket: "bucket"}}}
			return spec
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Project(build()); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestProjectAcceptsProjectOwnedAdditionalTeams(t *testing.T) {
	t.Parallel()
	spec := sentryv1alpha1.SentryProjectSpec{
		Team:        "payments",
		GrantAccess: &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}},
		IssueOwnership: &sentryv1alpha1.IssueOwnershipSpec{Rules: []sentryv1alpha1.IssueOwnershipRule{{
			Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"}, Teams: []string{"payments", "backend"},
		}}},
	}
	if err := Project(spec); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationMetricAlertValidation(t *testing.T) {
	t.Parallel()
	valid := sentryv1alpha1.SentryProjectSpec{
		Team: "payments",
		Alerts: &map[string]sentryv1alpha1.AlertSpec{"metric": {
			Metric: &sentryv1alpha1.MetricAlertSpec{
				Preset: "application-metric", IntervalMinutes: 5,
				ApplicationMetric: &sentryv1alpha1.ApplicationMetricExpression{Left: sentryv1alpha1.ApplicationMetricTerm{Name: "checkout.latency", Type: "distribution", Unit: "millisecond", Aggregation: "p95"}},
				Detection:         sentryv1alpha1.MetricDetection{Static: &sentryv1alpha1.StaticDetection{Direction: "above", High: 500}},
			},
			Actions: []sentryv1alpha1.AlertAction{{Email: &sentryv1alpha1.EmailAlertAction{Target: "suggested-assignees"}}},
		}},
	}
	if err := Project(valid); err != nil {
		t.Fatal(err)
	}
	if err := ProjectCompatibility(valid, sentryv1alpha1.AlertCompatibilityLegacy24); err == nil {
		t.Fatal("legacy compatibility accepted application metrics")
	}
	invalid := valid
	alerts := make(map[string]sentryv1alpha1.AlertSpec, len(*valid.Alerts))
	for key, value := range *valid.Alerts {
		alerts[key] = value
	}
	metric := alerts["metric"]
	metric.Metric = metric.Metric.DeepCopy()
	metric.Metric.ApplicationMetric.Left.Aggregation = "p95"
	metric.Metric.ApplicationMetric.Left.Type = "counter"
	alerts["metric"] = metric
	invalid.Alerts = &alerts
	if err := Project(invalid); err == nil {
		t.Fatal("invalid counter aggregation was accepted")
	}
}

func TestGitHubIssueActionValidation(t *testing.T) {
	t.Parallel()
	valid := sentryv1alpha1.SentryProjectSpec{
		Team: "payments",
		Alerts: &map[string]sentryv1alpha1.AlertSpec{"ticket": {
			Issue:   &sentryv1alpha1.IssueAlertSpec{Triggers: []sentryv1alpha1.IssueTrigger{{Type: "first-seen"}}},
			Actions: []sentryv1alpha1.AlertAction{{GitHubIssue: &sentryv1alpha1.GitHubIssueAlertAction{Integration: "company-github", Repository: "checkout", Labels: []string{"bug"}}}},
		}},
	}
	if err := Project(valid); err != nil {
		t.Fatal(err)
	}
	if err := ProjectCompatibility(valid, sentryv1alpha1.AlertCompatibilityModern26); err != nil {
		t.Fatal(err)
	}
	if err := ProjectCompatibility(valid, sentryv1alpha1.AlertCompatibilityLegacy24); err == nil {
		t.Fatal("legacy compatibility accepted a GitHub issue action")
	}
	invalid := valid
	alerts := make(map[string]sentryv1alpha1.AlertSpec, len(*valid.Alerts))
	for key, value := range *valid.Alerts {
		alerts[key] = value
	}
	alert := alerts["ticket"]
	alert.Actions = []sentryv1alpha1.AlertAction{{Email: &sentryv1alpha1.EmailAlertAction{Target: "suggested-assignees"}, GitHubIssue: &sentryv1alpha1.GitHubIssueAlertAction{Integration: "company-github", Repository: "checkout"}}}
	alerts["ticket"] = alert
	invalid.Alerts = &alerts
	if err := Project(invalid); err == nil {
		t.Fatal("ambiguous typed action was accepted")
	}
}

func TestNegativeProjectFixturesAreExecutableContracts(t *testing.T) {
	t.Parallel()
	directory := filepath.Join("..", "..", "testdata", "t01", "negative")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			project := &sentryv1alpha1.SentryProject{}
			err = yaml.UnmarshalStrict(content, project)
			if err == nil {
				err = Project(project.Spec)
			}
			if err == nil {
				t.Fatal("negative fixture was accepted")
			}
		})
	}
}
