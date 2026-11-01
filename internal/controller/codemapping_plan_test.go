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

package controller

import (
	"errors"
	"testing"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	codemappingruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/codemapping"
)

func TestCodeMappingDesiredResolvesOnlyConnectionOwnedAliases(t *testing.T) {
	integrations := &sentryv1alpha1.ConnectionIntegrations{SourceCode: &sentryv1alpha1.SourceCodeIntegrations{Installations: []sentryv1alpha1.SourceCodeIntegrationCatalogEntry{{Name: "github", Provider: "github", SentryName: "acme", Repositories: []sentryv1alpha1.SourceCodeRepositoryCatalogEntry{{Name: "checkout", SentryName: "acme/checkout"}}}}}}
	specs := map[string]sentryv1alpha1.CodeMappingSpec{"app": {Integration: "github", Repository: "checkout", StackRoot: "/", SourceRoot: "src", DefaultBranch: "main", ImportCodeowners: true}}
	desired, err := codeMappingDesired(specs, integrations)
	if err != nil {
		t.Fatal(err)
	}
	if desired["app"].IntegrationName != "acme" || desired["app"].RepositoryName != "acme/checkout" || desired["app"].Provider != "github" || !desired["app"].ImportCodeowners {
		t.Fatalf("desired=%#v", desired)
	}

	specs["app"] = sentryv1alpha1.CodeMappingSpec{Integration: "github", Repository: "missing"}
	if _, err := codeMappingDesired(specs, integrations); !errors.Is(err, codemappingruntime.ErrCatalogMissing) {
		t.Fatalf("err=%v", err)
	}
}
