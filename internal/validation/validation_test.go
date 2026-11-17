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
		return sentryv1alpha1.SentryProjectSpec{Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}}}
	}
	if err := Project(base()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() sentryv1alpha1.SentryProjectSpec{
		"create and use": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			slug := "existing"
			spec.Project.Use = &slug
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
		"unknown managed key": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.DSN = &sentryv1alpha1.DSNSpec{ManagedClientKey: "missing"}
			return spec
		},
		"use with managed settings": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			slug := "existing"
			spec.Project = sentryv1alpha1.ProjectSelector{Use: &slug}
			spec.Settings = &sentryv1alpha1.ProjectSettings{}
			return spec
		},
		"standby grant": func() sentryv1alpha1.SentryProjectSpec {
			spec := base()
			spec.Role = sentryv1alpha1.ProjectRoleStandby
			spec.GrantAccess = &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}}
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
		Project:     sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
		GrantAccess: &sentryv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}},
		IssueOwnership: &sentryv1alpha1.IssueOwnershipSpec{Rules: []sentryv1alpha1.IssueOwnershipRule{{
			Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"}, Teams: []string{"payments", "backend"},
		}}},
	}
	if err := Project(spec); err != nil {
		t.Fatal(err)
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
