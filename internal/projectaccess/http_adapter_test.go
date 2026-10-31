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

package projectaccess

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterUsesPublicProjectTeamEndpoints(t *testing.T) {
	t.Parallel()
	granted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/api/0/projects/company/checkout/teams/"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			teams := []teamResponse{{ID: "team-payments", Slug: "payments"}}
			if granted {
				teams = append(teams, teamResponse{ID: "team-backend", Slug: "backend"})
			}
			_ = json.NewEncoder(w).Encode(teams)
		case r.Method == http.MethodPost && r.URL.Path == base+"backend/":
			granted = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "project-id"})
		case r.Method == http.MethodDelete && r.URL.Path == base+"team-backend/":
			granted = false
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "project-id"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, Token: "test-token", CABundle: ca})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HTTPAdapter{Client: client}
	if err := adapter.GrantTeam(context.Background(), "company", "checkout", "backend"); err != nil {
		t.Fatal(err)
	}
	teams, err := adapter.ListTeams(context.Background(), "company", "checkout")
	if err != nil || len(teams) != 2 || teams[1].Identity != "team-backend" {
		t.Fatalf("teams=%#v err=%v", teams, err)
	}
	if err := adapter.RevokeTeam(context.Background(), "company", "checkout", "team-backend"); err != nil {
		t.Fatal(err)
	}
}
