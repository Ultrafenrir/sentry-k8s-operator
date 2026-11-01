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

package symbolsource

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterUsesPublicSymbolSourceCRUDContract(t *testing.T) {
	t.Parallel()
	methods := []string{}
	var payload Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"sko-source","name":"sko:source:uid","type":"http","url":"https://symbols.example.invalid","username":"reader","password":{"hidden-secret":true},"layout":{"type":"native","casing":"default"}}]`))
		case http.MethodPost, http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"id":"sko-source","name":"sko:source:uid","type":"http","url":"https://symbols.example.invalid","username":"reader","password":{"hidden-secret":true},"layout":{"type":"native","casing":"default"}}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HTTPAdapter{Client: client}
	items, err := adapter.List(context.Background(), "company", "checkout")
	if err != nil || len(items) != 1 || items[0].Identity != "sko-source" || items[0].URL != "https://symbols.example.invalid" {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	request := Request{Identity: "sko-source", Name: "sko:source:uid", Type: "http", Layout: Layout{Type: "native", Casing: "default"}, URL: "https://symbols.example.invalid", Username: "reader", Password: "SECRET-SENTINEL"}
	if _, err := adapter.Create(context.Background(), "company", "checkout", request); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Update(context.Background(), "company", "checkout", "sko-source", request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Delete(context.Background(), "company", "checkout", "sko-source"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{
		"GET /api/0/projects/company/checkout/symbol-sources/",
		"POST /api/0/projects/company/checkout/symbol-sources/",
		"PUT /api/0/projects/company/checkout/symbol-sources/?id=sko-source",
		"DELETE /api/0/projects/company/checkout/symbol-sources/?id=sko-source",
	}) || payload.Password != "SECRET-SENTINEL" {
		t.Fatalf("methods=%#v payload=%#v", methods, payload)
	}
}

func TestSourceDerivedPortableSymbolSourceFixtures(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		payload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/project-custom-symbol-sources.json")
		if err != nil {
			t.Fatal(err)
		}
		var requests []Request
		if err := json.Unmarshal(payload, &requests); err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if len(requests) != 3 || requests[0].Type != "http" || requests[1].Type != "s3" || requests[2].Type != "gcs" || requests[0].Password == "" || requests[1].SecretKey == "" || requests[2].PrivateKey == "" {
			t.Fatalf("%s fixture shape is incomplete", version)
		}
	}
}
