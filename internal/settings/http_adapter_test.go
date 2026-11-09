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

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
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

func TestHTTPAdapterRoundTripsBehaviorSettingsAsNarrowConditionalPatch(t *testing.T) {
	t.Parallel()

	var update map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("ETag", `"behavior-8"`)
			_, _ = w.Write([]byte(`{"resolveAge":24,"defaultEnvironment":"staging","storeCrashReports":0,"scrapeJavaScript":false,"relayPiiConfig":"{\"rules\":{},\"applications\":{}}","groupingEnhancements":"stack.module:\"vendor.*\" -group","fingerprintingRules":"message:\"timeout *\" -> timeout","builtinSymbolSources":["microsoft","nuget"],"allowedDomains":["preserve.example"]}`))
		case http.MethodPut:
			if r.Header.Get("If-Match") != `"behavior-8"` {
				t.Errorf("If-Match=%q", r.Header.Get("If-Match"))
			}
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("method=%q", r.Method)
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
	if err != nil || state.ResolveAge == nil || *state.ResolveAge != 24 || state.DefaultEnvironment == nil || *state.DefaultEnvironment != "staging" ||
		state.StoreCrashReports == nil || *state.StoreCrashReports != 0 || state.ScrapeJavaScript == nil || *state.ScrapeJavaScript ||
		state.RelayPIIConfig == nil || state.GroupingEnhancements == nil || state.FingerprintingRules == nil ||
		state.BuiltinSymbolSources == nil || !reflect.DeepEqual(*state.BuiltinSymbolSources, []string{"microsoft", "nuget"}) ||
		state.AllowedDomains == nil || !reflect.DeepEqual(*state.AllowedDomains, []string{"preserve.example"}) {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	resolve := int64(72)
	environment := "production"
	crashLimit := int32(-1)
	scrape := true
	allowedDomains := []string{"*.checkout.example.com", "https://checkout.example.com:443"}
	if err := adapter.UpdateProjectState(context.Background(), "company", "checkout", ProjectPatch{
		ResolveAge: &resolve, DefaultEnvironment: &environment,
		StoreCrashReports: &crashLimit, ScrapeJavaScript: &scrape,
		AllowedDomains: &allowedDomains,
	}, state.ETag); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(update, map[string]any{
		"resolveAge": float64(72), "defaultEnvironment": "production",
		"storeCrashReports": float64(-1), "scrapeJavaScript": true,
		"allowedDomains": []any{"*.checkout.example.com", "https://checkout.example.com:443"},
	}) {
		t.Fatalf("update=%#v", update)
	}
}

func TestHTTPAdapterDecodesLegacyBrowserStateAndWritesNarrowFilterPatches(t *testing.T) {
	t.Parallel()

	var updates []map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/0/projects/company/checkout/filters/" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":"filtered-transaction","active":false},{"id":"legacy-browsers","active":["chrome","ie"]}]`))
			return
		}
		if r.Method != http.MethodPut {
			t.Errorf("method=%q path=%q", r.Method, r.URL.Path)
			return
		}
		var update map[string]any
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Error(err)
		}
		updates = append(updates, update)
		w.WriteHeader(http.StatusNoContent)
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
	filters, err := adapter.ListFilters(context.Background(), "company", "checkout")
	if err != nil || len(filters) != 2 || filters[0].Active || !filters[1].SubfiltersPresent || !reflect.DeepEqual(filters[1].Subfilters, []string{"chrome", "ie"}) {
		t.Fatalf("filters=%#v err=%v", filters, err)
	}
	active := true
	if err := adapter.UpdateFilter(context.Background(), "company", "checkout", "filtered-transaction", FilterPatch{Active: &active}); err != nil {
		t.Fatal(err)
	}
	subfilters := []string{"ie", "chrome"}
	if err := adapter.UpdateFilter(context.Background(), "company", "checkout", "legacy-browsers", FilterPatch{Subfilters: &subfilters}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updates, []map[string]any{{"active": true}, {"subfilters": []any{"ie", "chrome"}}}) {
		t.Fatalf("updates=%#v", updates)
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
		advancedPayload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/project-advanced-settings-update-request.json")
		if err != nil {
			t.Fatal(err)
		}
		var advancedPatch ProjectPatch
		if err := json.Unmarshal(advancedPayload, &advancedPatch); err != nil {
			t.Fatalf("%s advanced: %v", version, err)
		}
		if advancedPatch.RelayPIIConfig == nil || advancedPatch.GroupingEnhancements == nil || advancedPatch.FingerprintingRules == nil ||
			advancedPatch.ScrubData != nil || advancedPatch.ResolveAge != nil {
			t.Fatalf("%s advanced patch=%#v", version, advancedPatch)
		}
		symbolPayload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/project-symbolication-update-request.json")
		if err != nil {
			t.Fatal(err)
		}
		var symbolPatch ProjectPatch
		if err := json.Unmarshal(symbolPayload, &symbolPatch); err != nil {
			t.Fatalf("%s symbolication: %v", version, err)
		}
		if symbolPatch.BuiltinSymbolSources == nil || !reflect.DeepEqual(*symbolPatch.BuiltinSymbolSources, []string{"electron", "microsoft", "nuget"}) ||
			symbolPatch.RelayPIIConfig != nil || symbolPatch.ResolveAge != nil {
			t.Fatalf("%s symbolication patch=%#v", version, symbolPatch)
		}
	}
}
