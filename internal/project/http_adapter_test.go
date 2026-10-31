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

package project

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterMatchesSourceDerivedCreateContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/0/teams/company/payments/projects/" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["name"] != "checkout [managed;o=payments;m=company-sentry-op]" || body["slug"] != "checkout" || body["default_rules"] != false {
			t.Errorf("create body = %#v", body)
		}
		if _, found := body["platform"]; found {
			t.Errorf("omitted platform was serialized: %#v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"internal-project-id","name":"checkout [managed;o=payments;m=company-sentry-op]","slug":"checkout","platform":null,"status":"active"}`))
	}))
	defer server.Close()
	adapter := &HTTPAdapter{Client: testHTTPClient(t, server)}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	remote, err := adapter.Create(context.Background(), CreateRequest{
		Organization: "company",
		Slug:         "checkout",
		Team:         "payments",
		DefaultRules: false,
		Marker:       marker,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if remote.Identity == "" || remote.Slug != "checkout" || remote.Marker == nil || *remote.Marker != marker {
		t.Fatalf("remote = %#v", remote)
	}
}

func TestHTTPAdapterGETParsesOnlyExactMarker(t *testing.T) {
	t.Parallel()

	responseName := "Checkout [managed;o=payments;m=company-sentry-op] trailing"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/0/projects/company/checkout/" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "project-id", "slug": "checkout", "name": responseName})
	}))
	defer server.Close()
	adapter := &HTTPAdapter{Client: testHTTPClient(t, server)}
	remote, err := adapter.GetBySlug(context.Background(), "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if remote.Marker != nil {
		t.Fatalf("lookalike marker was accepted: %#v", remote.Marker)
	}
	responseName = "Checkout [managed;o=payments;m=company-sentry-op]"
	remote, err = adapter.GetBySlug(context.Background(), "company", "checkout")
	if err != nil || remote.Marker == nil {
		t.Fatalf("exact marker remote=%#v err=%v", remote, err)
	}
}

func testHTTPClient(t *testing.T, server *httptest.Server) *sentryapi.Client {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, Token: "test-token", CABundle: ca})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
