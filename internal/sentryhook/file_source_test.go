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

package sentryhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFileSourceHandlerReloadsStrictConfigAndKeepsSourceLocal(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := strings.Repeat("a", 64)
	secret := []byte("source-secret-value")
	writeSourceConfig(t, directory, source, "checkout", secret)
	sink := &recordingSink{}
	now := time.Unix(1_800_000_000, 0)
	handler := &FileSourceHandler{Directory: directory, Sink: sink, MaxClockSkew: time.Minute, Now: func() time.Time { return now }}
	body := []byte(`{"project":{"slug":"checkout"},"event":{"eventID":"event-local"}}`)
	request := signedPublicRequest(source, body, secret, now)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || len(sink.envelopes) != 1 || sink.envelopes[0].SourceBindingKey != source {
		t.Fatalf("status=%d envelopes=%#v", response.Code, sink.envelopes)
	}

	rotated := []byte("rotated-source-secret")
	writeSourceConfig(t, directory, source, "checkout", secret, rotated)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, signedPublicRequest(source, body, secret, now))
	if response.Code != http.StatusNoContent || len(sink.envelopes) != 2 {
		t.Fatalf("overlap old secret status=%d calls=%d", response.Code, len(sink.envelopes))
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, signedPublicRequest(source, body, rotated, now))
	if response.Code != http.StatusNoContent || len(sink.envelopes) != 3 {
		t.Fatalf("overlap new secret status=%d calls=%d", response.Code, len(sink.envelopes))
	}

	writeSourceConfig(t, directory, source, "checkout", rotated)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, signedPublicRequest(source, body, secret, now))
	if response.Code != http.StatusUnauthorized || len(sink.envelopes) != 3 {
		t.Fatalf("stale secret status=%d calls=%d", response.Code, len(sink.envelopes))
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, signedPublicRequest(source, body, rotated, now))
	if response.Code != http.StatusNoContent || len(sink.envelopes) != 4 {
		t.Fatalf("rotated secret status=%d calls=%d", response.Code, len(sink.envelopes))
	}
}

func TestFileSourceHandlerRejectsTraversalAndMalformedConfigWithoutLeaks(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := strings.Repeat("b", 64)
	sentinel := "SENTINEL-MUST-NOT-LEAK"
	if err := os.WriteFile(filepath.Join(directory, source+".json"), []byte(`{"expectedProjectSlug":"checkout","hmacSecretsBase64":["`+sentinel+`"],"extra":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := &FileSourceHandler{Directory: directory, Sink: &recordingSink{}, MaxClockSkew: time.Minute}
	for _, path := range []string{"/events/../private", "/events/not-a-hash", PublicEventPrefix + source} {
		request := httptest.NewRequest(http.MethodPost, "https://receiver.invalid"+path, bytes.NewReader([]byte(`{}`)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusNoContent || strings.Contains(response.Body.String(), sentinel) {
			t.Fatalf("path=%q status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	first := []byte("first-overlap-secret")
	second := []byte("second-overlap-secret")
	for name, secrets := range map[string][][]byte{
		"empty":     {},
		"duplicate": {first, first},
		"too-many":  {first, second, []byte("third-overlap-secret")},
	} {
		t.Run(name, func(t *testing.T) {
			writeSourceConfig(t, directory, source, "checkout", secrets...)
			if _, _, err := loadFileSource(directory, source); err == nil {
				t.Fatal("invalid overlap set accepted")
			}
		})
	}
}

func TestFileSourceReadinessValidatesVisibleEntriesAndAllowsEmptyRotationBoundary(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	handler := &FileSourceHandler{Directory: directory, Sink: &recordingSink{}}
	if err := handler.Ready(); err != nil {
		t.Fatalf("empty source directory not ready: %v", err)
	}
	source := strings.Repeat("c", 64)
	writeSourceConfig(t, directory, source, "checkout", []byte("valid-source-secret"))
	if err := handler.Ready(); err != nil {
		t.Fatalf("valid source directory not ready: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "..data"), []byte("projected-volume-internal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handler.Ready(); err != nil {
		t.Fatalf("projected internal entry affected readiness: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unexpected.txt"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handler.Ready(); err == nil {
		t.Fatal("unexpected visible entry left receiver ready")
	}
}

func writeSourceConfig(t *testing.T, directory, source, slug string, secrets ...[]byte) {
	t.Helper()
	encoded := make([]string, len(secrets))
	for index, secret := range secrets {
		encoded[index] = base64.StdEncoding.EncodeToString(secret)
	}
	payload, err := json.Marshal(VerifierConfig{ExpectedProjectSlug: slug, HMACSecretsBase64: encoded})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, source+".json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func signedPublicRequest(source string, body, secret []byte, now time.Time) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "https://receiver.invalid"+PublicEventPrefix+source, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(TimestampHeader, strconv.FormatInt(now.Unix(), 10))
	request.Header.Set(GUIDHeader, "fixture-guid")
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	request.Header.Set(SignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	return request
}
