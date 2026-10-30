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

package dsn

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestKeyReaderCollectsEverySentryPage(t *testing.T) {
	t.Parallel()

	requests := 0
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/0/projects/company/checkout/keys/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?cursor=next>; rel="next"; results="true"; cursor="next"`, server.URL, r.URL.Path))
			_, _ = w.Write([]byte(`[{"id":"inactive","name":"Old","isActive":false,"dsn":{"public":"https://old@sentry.example/1"}}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?cursor=done>; rel="next"; results="false"; cursor="done"`, server.URL, r.URL.Path))
		_, _ = w.Write([]byte(`[{"id":"active","name":"Default","isActive":true,"dsn":{"public":"https://public@sentry.example/1"}}]`))
	}))
	defer server.Close()
	certificate := server.Certificate()
	caBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, CABundle: caBundle})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := (&KeyReader{Client: client}).List(context.Background(), "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(keys) != 2 || keys[1].ID != "active" {
		t.Fatalf("requests=%d keys=%#v", requests, keys)
	}
}

func TestSourceDerivedClientKeyFixturesSelectOneActive(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		payload, err := os.ReadFile("../../testdata/http-fixtures/" + version + "/client-keys-response.json")
		if err != nil {
			t.Fatal(err)
		}
		var keys []ClientKey
		if err := json.Unmarshal(payload, &keys); err != nil {
			t.Fatal(err)
		}
		selection, err := Select(keys, "", nil)
		if err != nil {
			t.Fatalf("%s Select: %v", version, err)
		}
		if selection.Key.ID == "" || selection.Binding.KeyName != "Default" || selection.Binding.SelectorHash == "" {
			t.Fatalf("%s selection = %#v", version, selection)
		}
	}
}

func TestSelectRejectsAmbiguityAndUsesReadableSelectorOnly(t *testing.T) {
	t.Parallel()

	keys := []ClientKey{
		clientKey("id-one", "Default", true, "https://public-one@sentry.example/1"),
		clientKey("id-two", "Default", true, "https://public-two@sentry.example/1"),
	}
	if _, err := Select(keys, "", nil); !errors.Is(err, ErrKeyAmbiguous) {
		t.Fatalf("multiple active error = %v", err)
	}
	if _, err := Select(keys, "Default", nil); !errors.Is(err, ErrKeyAmbiguous) {
		t.Fatalf("duplicate label error = %v", err)
	}
	if _, err := Select(keys, "id-one", nil); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("external ID was accepted as selector: %v", err)
	}
	keys[1].Label = "Secondary"
	selection, err := Select(keys, "Secondary", nil)
	if err != nil || selection.Key.ID != "id-two" {
		t.Fatalf("explicit selection = %#v, %v", selection, err)
	}
}

func TestSelectAllowsHTTPDSNOnlyWithExplicitTransportOptIn(t *testing.T) {
	t.Parallel()

	keys := []ClientKey{clientKey("id-one", "Default", true, "http://public@sentry.internal/1")}
	if _, err := Select(keys, "", nil); !errors.Is(err, ErrInvalidDSN) {
		t.Fatalf("secure selection accepted HTTP DSN: %v", err)
	}
	selection, err := SelectWithTransport(keys, "", nil, true)
	if err != nil || selection.Key.ID != "id-one" {
		t.Fatalf("explicit insecure HTTP selection = %#v, %v", selection, err)
	}
}

func TestPinnedKeyDoesNotSwitchAfterRenameOrReplacement(t *testing.T) {
	t.Parallel()

	first := clientKey("id-one", "Default", true, "https://public@sentry.example/1")
	selection, err := Select([]ClientKey{first}, "Default", nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Label = "Renamed"
	selection, err = Select([]ClientKey{first}, "Default", &selection.Binding)
	if err != nil || !selection.ExternalRenamed || selection.Key.ID != "id-one" || selection.ReadableName != "Renamed" || selection.Binding.KeyName != "Default" {
		t.Fatalf("renamed pin = %#v, %v", selection, err)
	}
	selection, err = Select([]ClientKey{first}, "Default", &selection.Binding)
	if err != nil || !selection.ExternalRenamed || selection.Binding.KeyName != "Default" {
		t.Fatalf("persistent rename signal = %#v, %v", selection, err)
	}
	replacement := clientKey("id-two", "Default", true, "https://other@sentry.example/1")
	if _, err := Select([]ClientKey{replacement}, "Default", &selection.Binding); !errors.Is(err, ErrPinnedKeyLost) {
		t.Fatalf("same-name replacement error = %v", err)
	}
}

func TestSelectRejectsUnsafeReadableName(t *testing.T) {
	t.Parallel()

	key := clientKey("id", "unsafe\nname", true, "https://public@sentry.example/1")
	if _, err := Select([]ClientKey{key}, "", nil); !errors.Is(err, ErrInvalidKeyName) {
		t.Fatalf("unsafe initial name error = %v", err)
	}
	key.Label = "Default"
	selection, err := Select([]ClientKey{key}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	key.Label = "renamed\x00key"
	if _, err := Select([]ClientKey{key}, "", &selection.Binding); !errors.Is(err, ErrInvalidKeyName) {
		t.Fatalf("unsafe renamed key error = %v", err)
	}
}

func TestSelectRejectsInactiveAndSecretBearingDSN(t *testing.T) {
	t.Parallel()

	inactive := clientKey("id", "Default", false, "https://public@sentry.example/1")
	if _, err := Select([]ClientKey{inactive}, "", nil); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("inactive key error = %v", err)
	}
	secretDSN := clientKey("id", "Default", true, "https://public:secret@sentry.example/1")
	if _, err := Select([]ClientKey{secretDSN}, "", nil); !errors.Is(err, ErrInvalidDSN) {
		t.Fatalf("secret-bearing DSN error = %v", err)
	}
}

func clientKey(id, label string, active bool, publicDSN string) ClientKey {
	key := ClientKey{ID: id, Name: label, Label: label, IsActive: active}
	key.DSN.Public = publicDSN
	return key
}
