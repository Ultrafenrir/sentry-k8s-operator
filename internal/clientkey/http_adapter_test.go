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

package clientkey

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterUsesPublicLifecycleContractAndDropsSecrets(t *testing.T) {
	t.Parallel()

	requests := make([]string, 0, 5)
	bodies := make([]map[string]any, 0, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			bodies = append(bodies, body)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/0/projects/company/checkout/keys/":
			w.Header().Set("Link", "")
			_, _ = w.Write([]byte(`[{"id":"public-id","name":"owned","isActive":true,"rateLimit":null,"secret":"SECRET-SENTINEL","dsn":{"public":"https://public@example.invalid/1","secret":"https://public:SECRET-SENTINEL@example.invalid/1"}}]`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(remoteFixture))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(remoteFixture))
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte(remoteFixture))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := &HTTPAdapter{Client: testClient(t, server)}
	listed, err := adapter.List(context.Background(), "company", "checkout")
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	encoded, _ := json.Marshal(listed)
	if strings.Contains(string(encoded), "SECRET-SENTINEL") {
		t.Fatalf("secret material survived allowlisted decode: %s", encoded)
	}
	rate := &RateLimit{Count: 1000, Window: 60}
	created, err := adapter.Create(context.Background(), "company", "checkout", CreateRequest{Name: "owned", RateLimit: rate})
	if err != nil || created.Identity != "public-id" || created.DSN.Public == "" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if _, found, err := adapter.Get(context.Background(), "company", "checkout", "public-id"); err != nil || !found {
		t.Fatalf("get found=%t err=%v", found, err)
	}
	if _, err := adapter.Update(context.Background(), "company", "checkout", "public-id", UpdateRequest{Name: "owned", IsActive: false}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(context.Background(), "company", "checkout", "public-id"); err != nil {
		t.Fatal(err)
	}
	wantRequests := []string{
		"GET /api/0/projects/company/checkout/keys/",
		"POST /api/0/projects/company/checkout/keys/",
		"GET /api/0/projects/company/checkout/keys/public-id/",
		"PUT /api/0/projects/company/checkout/keys/public-id/",
		"DELETE /api/0/projects/company/checkout/keys/public-id/",
	}
	if !reflect.DeepEqual(requests, wantRequests) {
		t.Fatalf("requests=%#v", requests)
	}
	if len(bodies) != 2 || bodies[0]["name"] != "owned" || bodies[0]["public"] != nil || bodies[0]["secret"] != nil || bodies[1]["isActive"] != false {
		t.Fatalf("bodies=%#v", bodies)
	}
}

func TestHTTPAdapterTreatsDetailNotFoundAsAbsent(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	_, found, err := (&HTTPAdapter{Client: testClient(t, server)}).Get(context.Background(), "company", "checkout", "missing")
	if err != nil || found {
		t.Fatalf("found=%t err=%v", found, err)
	}
}

func testClient(t *testing.T, server *httptest.Server) *sentryapi.Client {
	t.Helper()
	client, err := sentryapi.NewClient(sentryapi.Config{
		BaseURL:  server.URL,
		CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

const remoteFixture = `{"id":"public-id","name":"owned","isActive":true,"rateLimit":{"count":1000,"window":60},"dateCreated":"2026-10-31T00:00:00Z","secret":"SECRET-SENTINEL","dsn":{"public":"https://public@example.invalid/1","secret":"https://public:SECRET-SENTINEL@example.invalid/1"}}`
