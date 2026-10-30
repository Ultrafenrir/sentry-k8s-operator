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
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

const fixtureSecret = "fixture-only-not-a-secret"

type recordingSink struct {
	envelopes []Envelope
	err       error
}

func (s *recordingSink) Accept(_ context.Context, envelope Envelope) error {
	s.envelopes = append(s.envelopes, envelope)
	return s.err
}

func TestHandlerAuthenticatesSourceBeforeParsingAndDurablyAcceptsFixtures(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		version := version
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			body, headers, timestamp := fixture(t, version)
			sink := &recordingSink{}
			handler := testHandler(timestamp, sink, func(context.Context) ([]byte, error) {
				return []byte(fixtureSecret), nil
			})

			response := serve(handler, body, headers)
			if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
				t.Fatalf("valid fixture response = %d %q", response.Code, response.Body.String())
			}
			if len(sink.envelopes) != 1 || sink.envelopes[0].SourceBindingKey != "binding-local-key" || len(sink.envelopes[0].DedupeKey) != 64 {
				t.Fatalf("envelopes = %+v", sink.envelopes)
			}

			unsigned := headers.Clone()
			unsigned.Del(SignatureHeader)
			response = serve(handler, []byte("{"), unsigned)
			if response.Code != http.StatusUnauthorized || len(sink.envelopes) != 1 {
				t.Fatalf("unsigned malformed response = %d, calls = %d", response.Code, len(sink.envelopes))
			}

			tampered := append([]byte(nil), body...)
			tampered[len(tampered)-1] ^= 1
			if response = serve(handler, tampered, headers); response.Code != http.StatusUnauthorized {
				t.Fatalf("tampered response = %d", response.Code)
			}
		})
	}
}

func TestHandlerRetriesSinkAndLeavesReplayDeduplicationToDurableBoundary(t *testing.T) {
	t.Parallel()
	body, headers, timestamp := fixture(t, "sentry-24.7.1")
	sink := &recordingSink{err: ErrSink}
	handler := testHandler(timestamp, sink, func(context.Context) ([]byte, error) { return []byte(fixtureSecret), nil })
	if response := serve(handler, body, headers); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("first response = %d", response.Code)
	}
	sink.err = nil
	if response := serve(handler, body, headers); response.Code != http.StatusNoContent {
		t.Fatalf("retry response = %d", response.Code)
	}
	if len(sink.envelopes) != 2 || sink.envelopes[0] != sink.envelopes[1] {
		t.Fatalf("retry envelopes = %+v", sink.envelopes)
	}
	modifiedHeaders := headers.Clone()
	modifiedHeaders.Set(GUIDHeader, "attacker-controlled-replay-guid")
	if response := serve(handler, body, modifiedHeaders); response.Code != http.StatusNoContent {
		t.Fatalf("replay response = %d", response.Code)
	}
	if len(sink.envelopes) != 3 || sink.envelopes[0].DedupeKey != sink.envelopes[2].DedupeKey {
		t.Fatalf("mutable headers changed signed-body dedupe key: %+v", sink.envelopes)
	}
	rotatedSecret := []byte("rotated-fixture-secret")
	rotatedHeaders := headers.Clone()
	mac := hmac.New(sha256.New, rotatedSecret)
	_, _ = mac.Write(body)
	rotatedHeaders.Set(SignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	rotatedHandler := testHandler(timestamp, sink, func(context.Context) ([]byte, error) { return rotatedSecret, nil })
	if response := serve(rotatedHandler, body, rotatedHeaders); response.Code != http.StatusNoContent {
		t.Fatalf("rotated-secret replay response = %d", response.Code)
	}
	if len(sink.envelopes) != 4 || sink.envelopes[0].DedupeKey != sink.envelopes[3].DedupeKey {
		t.Fatalf("HMAC rotation changed authenticated-event dedupe key: %+v", sink.envelopes)
	}
}

func TestHandlerReloadsSecretAndRejectsBadEnvelopeWithoutLeaks(t *testing.T) {
	t.Parallel()
	body, headers, timestamp := fixture(t, "sentry-24.7.1")
	secret := []byte("wrong-secret-value")
	loads := 0
	sink := &recordingSink{}
	handler := testHandler(timestamp, sink, func(context.Context) ([]byte, error) {
		loads++
		return append([]byte(nil), secret...), nil
	})
	if response := serve(handler, body, headers); response.Code != http.StatusUnauthorized || response.Body.Len() != 0 {
		t.Fatalf("wrong-secret response = %d %q", response.Code, response.Body.String())
	}
	secret = []byte(fixtureSecret)
	if response := serve(handler, body, headers); response.Code != http.StatusNoContent {
		t.Fatalf("rotated-secret response = %d", response.Code)
	}
	if loads != 2 || len(sink.envelopes) != 1 {
		t.Fatalf("loads = %d, envelopes = %d", loads, len(sink.envelopes))
	}

	stale := headers.Clone()
	stale.Set(TimestampHeader, "1")
	if response := serve(handler, body, stale); response.Code != http.StatusUnauthorized {
		t.Fatalf("stale response = %d", response.Code)
	}

	oversized := bytes.Repeat([]byte("x"), 1025)
	handler.MaxBodyBytes = 1024
	if response := serve(handler, oversized, headers); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized response = %d", response.Code)
	}

	duplicateSignature := headers.Clone()
	duplicateSignature.Add(SignatureHeader, headers.Get(SignatureHeader))
	handler.MaxBodyBytes = 0
	if response := serve(handler, body, duplicateSignature); response.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate signature response = %d", response.Code)
	}
}

func TestHandlerRejectsValidSignatureForUnexpectedProject(t *testing.T) {
	t.Parallel()
	body, headers, timestamp := fixture(t, "sentry-24.7.1")
	handler := testHandler(timestamp, &recordingSink{}, func(context.Context) ([]byte, error) { return []byte(fixtureSecret), nil })
	handler.ExpectedProjectSlug = "another-project"
	response := serve(handler, body, headers)
	if response.Code != http.StatusBadRequest || response.Body.Len() != 0 {
		t.Fatalf("unexpected-project response = %d %q", response.Code, response.Body.String())
	}
}

func testHandler(now time.Time, sink Sink, secret SecretSource) *Handler {
	return &Handler{
		Secret: secret, Sink: sink, Path: "/events", SourceBindingKey: "binding-local-key",
		ExpectedProjectSlug: "checkout", MaxClockSkew: 5 * time.Minute, Now: func() time.Time { return now },
	}
}

func serve(handler http.Handler, body []byte, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "https://receiver.invalid/events", bytes.NewReader(body))
	request.Header = headers.Clone()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func fixture(t *testing.T, version string) ([]byte, http.Header, time.Time) {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "http-fixtures", version)
	body, err := os.ReadFile(filepath.Join(root, "service-hook-event-alert.body.json"))
	if err != nil {
		t.Fatal(err)
	}
	headerData, err := os.ReadFile(filepath.Join(root, "service-hook-event-alert.headers.json"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(headerData, &values); err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	for name, value := range values {
		headers.Set(name, value)
	}
	timestampSeconds, err := strconv.ParseInt(values[TimestampHeader], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return body, headers, time.Unix(timestampSeconds, 0)
}

func TestHandlerConfigurationFailuresAreBodyless(t *testing.T) {
	t.Parallel()
	handler := &Handler{Path: "/events"}
	request := httptest.NewRequest(http.MethodPost, "https://receiver.invalid/events", strings.NewReader("private-payload-SHOULD-NOT-LEAK"))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "SHOULD-NOT-LEAK") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}

}
