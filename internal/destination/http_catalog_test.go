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

package destination

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPCatalogReadsVersionPinnedPagerDutyServiceTable(t *testing.T) {
	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		version := version
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			payload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/pagerduty-integrations-response.json")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/0/organizations/company/integrations/" || r.URL.Query().Get("providerKey") != "pagerduty" || r.URL.Query().Get("includeConfig") != "1" {
					t.Errorf("request = %s %s", r.Method, r.URL.String())
				}
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
			if err != nil {
				t.Fatal(err)
			}
			catalog := &HTTPCatalog{Client: client}
			integrations, err := catalog.ListIntegrations(context.Background(), "company", "pagerduty")
			if err != nil {
				t.Fatal(err)
			}
			if len(integrations) != 1 || integrations[0].Identity != "1001" || integrations[0].Name != "Primary" {
				t.Fatalf("integrations=%#v", integrations)
			}
			services, err := catalog.ListPagerDutyServices(context.Background(), "company", "1001")
			if err != nil || len(services) != 2 || services[0].Identity != "2001" || services[0].Name != "Checkout" {
				t.Fatalf("services=%#v err=%v", services, err)
			}
		})
	}
}

func TestHTTPCatalogListsSlackChannelsFromSentryResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/0/organizations/company/integrations/1001/channels/" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"C001","name":"backend-alerts","display":"#backend-alerts","type":"public"},{"id":"C002","name":"incidents","display":"#incidents","type":"private"}]}`))
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := (&HTTPCatalog{Client: client}).ListSlackChannels(context.Background(), "company", "1001")
	if err != nil || len(channels) != 2 || channels[0].Identity != "C001" || channels[0].Name != "backend-alerts" || channels[1].Identity != "C002" {
		t.Fatalf("channels=%#v err=%v", channels, err)
	}
}
