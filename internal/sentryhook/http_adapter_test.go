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

package sentryhook

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPServiceHookAdapterLifecycleContract(t *testing.T) {
	t.Parallel()
	const endpoint = "https://hooks.example.test/events/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/api/0/projects/company/checkout/hooks/" {
				t.Errorf("create request = %s %s", request.Method, request.URL.Path)
			}
			assertServiceHookRequest(t, request, endpoint, false)
			writer.WriteHeader(http.StatusCreated)
		case 2:
			if request.Method != http.MethodGet || request.URL.Path != "/api/0/projects/company/checkout/hooks/hook-id/" {
				t.Errorf("get request = %s %s", request.Method, request.URL.Path)
			}
		case 3:
			if request.Method != http.MethodPut || request.URL.Path != "/api/0/projects/company/checkout/hooks/hook-id/" {
				t.Errorf("repair request = %s %s", request.Method, request.URL.Path)
			}
			assertServiceHookRequest(t, request, endpoint, true)
		case 4:
			if request.Method != http.MethodDelete || request.URL.Path != "/api/0/projects/company/checkout/hooks/hook-id/" {
				t.Errorf("delete request = %s %s", request.Method, request.URL.Path)
			}
			writer.WriteHeader(http.StatusNoContent)
			return
		default:
			t.Errorf("unexpected request %d", requests)
		}
		_ = json.NewEncoder(writer).Encode(serviceHookResponse{ID: "hook-id", URL: endpoint, Secret: "0123456789abcdef0123456789abcdef", Events: []string{AlertEvent}, Status: "active"})
	}))
	defer server.Close()
	adapter := &HTTPServiceHookAdapter{Client: serviceHookTestClient(t, server)}
	created, err := adapter.Create(context.Background(), "company", "checkout", endpoint, false)
	if err != nil || created.Identity != "hook-id" || !created.Active || string(created.Secret) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if observed, found, err := adapter.Get(context.Background(), "company", "checkout", "hook-id"); err != nil || !found || observed.Identity != created.Identity {
		t.Fatalf("observed=%#v found=%v err=%v", observed, found, err)
	}
	if repaired, err := adapter.Repair(context.Background(), "company", "checkout", "hook-id", endpoint); err != nil || !repaired.Active {
		t.Fatalf("repaired=%#v err=%v", repaired, err)
	}
	if err := adapter.Delete(context.Background(), "company", "checkout", "hook-id"); err != nil {
		t.Fatal(err)
	}
}

func assertServiceHookRequest(t *testing.T, request *http.Request, endpoint string, active bool) {
	t.Helper()
	var body serviceHookRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.URL != endpoint || len(body.Events) != 1 || body.Events[0] != AlertEvent || body.Version != 0 || body.IsActive != active {
		t.Fatalf("request body = %#v", body)
	}
}

func serviceHookTestClient(t *testing.T, server *httptest.Server) *sentryapi.Client {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, Token: "test-token", CABundle: ca})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
