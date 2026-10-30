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

package sentryapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCollectPagesFollowsSentryNextLinks(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/0/projects/?cursor=next>; rel="next"; results="true"; cursor="next"`, server.URL))
			_, _ = w.Write([]byte(`[{"slug":"one"}]`))
		case "next":
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/0/projects/?cursor=done>; rel="next"; results="false"; cursor="done"`, server.URL))
			_, _ = w.Write([]byte(`[{"slug":"two"}]`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server)})
	type project struct {
		Slug string `json:"slug"`
	}
	projects, err := CollectPages[project](context.Background(), client, "/api/0/projects/", "list-projects")
	if err != nil {
		t.Fatalf("CollectPages: %v", err)
	}
	if len(projects) != 2 || projects[0].Slug != "one" || projects[1].Slug != "two" {
		t.Fatalf("projects = %#v", projects)
	}
}

func TestCollectPagesRejectsExternalNextLink(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://attacker.example/steal>; rel="next"; results="true"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, Token: "never-leak", CABundle: serverCA(t, server)})
	_, err := CollectPages[struct{}](context.Background(), client, "/", "list")
	if !IsKind(err, ErrorInvalid) {
		t.Fatalf("error = %v, want Invalid", err)
	}
}

func TestNextPageRejectsMalformedLink(t *testing.T) {
	t.Parallel()

	if _, err := nextPage([]string{`missing-angle>; rel="next"; results="true"`}); err == nil {
		t.Fatal("malformed next link was accepted")
	}
}
