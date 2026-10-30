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
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverCapabilitiesPreservesEvidenceStates(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/available":
			w.WriteHeader(http.StatusNoContent)
		case "/absent":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server)})
	results, err := DiscoverCapabilities(context.Background(), client, []CapabilityProbe{
		{Name: "unknown", Path: "/absent"},
		{Name: "available", Path: "/available"},
		{Name: "unsupported", Path: "/absent", NotFoundMeansUnavailable: true},
	})
	if err != nil {
		t.Fatalf("DiscoverCapabilities: %v", err)
	}
	want := []CapabilityResult{
		{Name: "available", State: CapabilityAvailable},
		{Name: "unknown", State: CapabilityUnknown},
		{Name: "unsupported", State: CapabilityUnavailable},
	}
	for i := range want {
		if results[i] != want[i] {
			t.Fatalf("results[%d] = %#v, want %#v", i, results[i], want[i])
		}
	}
}

func TestDiscoverCapabilitiesDoesNotTurn403IntoUnsupported(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server)})
	_, err := DiscoverCapabilities(context.Background(), client, []CapabilityProbe{{Name: "private", Path: "/private", NotFoundMeansUnavailable: true}})
	if !IsKind(err, ErrorPermissionDenied) {
		t.Fatalf("403 error = %v, want PermissionDenied", err)
	}
}

func TestDiscoverCapabilitiesRejectsMutationProbe(t *testing.T) {
	t.Parallel()

	client := mustClient(t, Config{BaseURL: "https://sentry.example"})
	_, err := DiscoverCapabilities(context.Background(), client, []CapabilityProbe{{Name: "unsafe", Path: "/", Method: http.MethodPost}})
	if err == nil {
		t.Fatal("mutating capability probe was accepted")
	}
}
