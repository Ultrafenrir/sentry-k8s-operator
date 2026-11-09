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

package codemapping

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterUsesPortableRepositoryMappingAndCodeownersContracts(t *testing.T) {
	t.Parallel()
	methods := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/0/organizations/company/integrations/":
			_, _ = w.Write([]byte(`[{"id":11,"name":"acme","provider":{"key":"github"}}]`))
		case "/api/0/organizations/company/repos/":
			_, _ = w.Write([]byte(`[{"id":22,"name":"acme/checkout","integrationId":11}]`))
		case "/api/0/organizations/company/code-mappings/":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[{"id":33,"projectId":44,"projectSlug":"checkout","repoId":22,"repoName":"acme/checkout","integrationId":11,"provider":{"key":"github"},"stackRoot":"/","sourceRoot":"src","defaultBranch":"main"}]`))
			} else {
				assertMappingRequest(t, r)
				_, _ = w.Write([]byte(`{"id":33,"projectId":44,"projectSlug":"checkout","repoId":22,"integrationId":11,"provider":{"key":"github"},"stackRoot":"/","sourceRoot":"src","defaultBranch":"main"}`))
			}
		case "/api/0/organizations/company/code-mappings/33/":
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
			} else {
				assertMappingRequest(t, r)
				_, _ = w.Write([]byte(`{"id":33,"projectId":44,"repoId":22,"integrationId":11,"provider":{"key":"github"}}`))
			}
		case "/api/0/organizations/company/code-mappings/33/codeowners/":
			_, _ = w.Write([]byte(`{"raw":"src/* @acme/backend"}`))
		case "/api/0/projects/company/checkout/codeowners/":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[{"id":55,"raw":"src/* @acme/backend","codeMappingId":33}]`))
			} else {
				assertCodeownersRequest(t, r)
				_, _ = w.Write([]byte(`{"id":55,"raw":"src/* @acme/backend","codeMappingId":33}`))
			}
		case "/api/0/projects/company/checkout/codeowners/55/":
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
			} else {
				assertCodeownersRequest(t, r)
				_, _ = w.Write([]byte(`{"id":55,"raw":"src/* @acme/backend","codeMappingId":33}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	a := &HTTPAdapter{Client: client}
	ctx := context.Background()
	if values, err := a.ListIntegrations(ctx, "company", "github"); err != nil || len(values) != 1 || values[0].Identity != "11" {
		t.Fatalf("integrations=%#v err=%v", values, err)
	}
	if values, err := a.ListRepositories(ctx, "company", "11"); err != nil || len(values) != 1 || values[0].Identity != "22" {
		t.Fatalf("repositories=%#v err=%v", values, err)
	}
	if values, err := a.ListMappings(ctx, "company"); err != nil || len(values) != 1 || values[0].Identity != "33" {
		t.Fatalf("mappings=%#v err=%v", values, err)
	}
	request := MappingRequest{IntegrationID: "11", RepositoryID: "22", ProjectID: "44", StackRoot: "/", SourceRoot: "src", DefaultBranch: "main"}
	if _, err := a.CreateMapping(ctx, "company", request); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateMapping(ctx, "company", "33", request); err != nil {
		t.Fatal(err)
	}
	if source, err := a.FetchCodeowners(ctx, "company", "33"); err != nil || source.Raw == "" {
		t.Fatalf("source=%#v err=%v", source, err)
	}
	if values, err := a.ListCodeowners(ctx, "company", "checkout"); err != nil || len(values) != 1 || values[0].Identity != "55" {
		t.Fatalf("codeowners=%#v err=%v", values, err)
	}
	if _, err := a.CreateCodeowners(ctx, "company", "checkout", "33", "src/* @acme/backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateCodeowners(ctx, "company", "checkout", "55", "33", "src/* @acme/backend"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteCodeowners(ctx, "company", "checkout", "55"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteMapping(ctx, "company", "33"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/0/organizations/company/integrations/?provider_key=github", "GET /api/0/organizations/company/repos/?integration_id=11", "GET /api/0/organizations/company/code-mappings/", "POST /api/0/organizations/company/code-mappings/", "PUT /api/0/organizations/company/code-mappings/33/", "GET /api/0/organizations/company/code-mappings/33/codeowners/", "GET /api/0/projects/company/checkout/codeowners/", "POST /api/0/projects/company/checkout/codeowners/", "PUT /api/0/projects/company/checkout/codeowners/55/", "DELETE /api/0/projects/company/checkout/codeowners/55/", "DELETE /api/0/organizations/company/code-mappings/33/"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods=%#v", methods)
	}
}

func assertMappingRequest(t *testing.T, r *http.Request) {
	t.Helper()
	var request MappingRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request.IntegrationID != "11" || request.RepositoryID != "22" || request.ProjectID != "44" || request.DefaultBranch != "main" {
		t.Fatalf("mapping request=%#v", request)
	}
}

func assertCodeownersRequest(t *testing.T, r *http.Request) {
	t.Helper()
	var request map[string]string
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request["codeMappingId"] != "33" || request["raw"] != "src/* @acme/backend" {
		t.Fatalf("CODEOWNERS request=%#v", request)
	}
}
