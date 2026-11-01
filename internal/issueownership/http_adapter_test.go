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

package issueownership

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterReadsNullableRawAndWritesExactPublicContract(t *testing.T) {
	t.Parallel()

	requests := 0
	var update map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/0/projects/company/checkout/ownership/" {
			t.Errorf("path=%q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"raw":null,"fallthrough":true,"autoAssignment":"Auto Assign to Issue Owner","codeownersAutoSync":true,"schema":null}`))
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"raw":"path:src/* #checkout","fallthrough":false,"autoAssignment":"Turn off Auto-Assignment","codeownersAutoSync":false}`))
		default:
			t.Errorf("method=%q", r.Method)
		}
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{
		BaseURL: server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HTTPAdapter{Client: client}
	observed, err := adapter.Get(context.Background(), "company", "checkout")
	if err != nil || observed.Raw == nil || *observed.Raw != "" || observed.Fallthrough == nil || !*observed.Fallthrough {
		t.Fatalf("observed=%#v err=%v", observed, err)
	}
	patch := Patch{Raw: "path:src/* #checkout", Fallthrough: false, AutoAssignment: autoAssignDisabled, CodeownersAutoSync: false}
	if err := adapter.Update(context.Background(), "company", "checkout", patch); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"raw": "path:src/* #checkout", "fallthrough": false,
		"autoAssignment": autoAssignDisabled, "codeownersAutoSync": false,
	}
	if requests != 2 || !reflect.DeepEqual(update, want) {
		t.Fatalf("requests=%d update=%#v", requests, update)
	}
}
