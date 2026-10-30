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

package settings

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterFreshGETThenConditionalNarrowUpdate(t *testing.T) {
	t.Parallel()

	requests := 0
	var update map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/0/projects/company/checkout/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("ETag", `"revision-9"`)
			_, _ = w.Write([]byte(`{"dataScrubber":false,"dataScrubberDefaults":true,"scrubIPAddresses":false,"sensitiveFields":["manual"],"safeFields":["neighbor"],"relayPiiConfig":"preserve"}`))
		case http.MethodPut:
			if r.Header.Get("If-Match") != `"revision-9"` {
				t.Errorf("If-Match = %q", r.Header.Get("If-Match"))
			}
			payload, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(payload, &update); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("method = %q", r.Method)
		}
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{
		BaseURL:  server.URL,
		CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HTTPAdapter{Client: client}
	state, err := adapter.GetProjectState(context.Background(), "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	trueValue := true
	empty := []string{}
	if err := adapter.UpdateProjectState(context.Background(), "company", "checkout", ProjectPatch{ScrubData: &trueValue, SensitiveFields: &empty}, state.ETag); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !reflect.DeepEqual(update, map[string]any{"dataScrubber": true, "sensitiveFields": []any{}}) {
		t.Fatalf("requests=%d update=%#v", requests, update)
	}
}

func TestSourceDerivedSettingsFixturesDecodeAsNarrowPatches(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		payload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/project-settings-update-request.json")
		if err != nil {
			t.Fatal(err)
		}
		var patch ProjectPatch
		if err := json.Unmarshal(payload, &patch); err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		if patch.ScrubData == nil || !*patch.ScrubData || patch.SensitiveFields == nil || len(*patch.SensitiveFields) != 2 {
			t.Fatalf("%s patch=%#v", version, patch)
		}
	}
}
