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

package telegramdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

const testToken = "123456:test_token"

func TestClientSendsNumericTargetWithoutLinkPreview(t *testing.T) {
	t.Parallel()
	var received sendMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/bot"+testToken+"/sendMessage" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()

	client := testClient(server)
	err := client.Send(context.Background(), binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}, "safe summary")
	if err != nil {
		t.Fatal(err)
	}
	if received.ChatID != -100123 || received.MessageThreadID == nil || *received.MessageThreadID != 42 || received.Text != "safe summary" || !received.LinkPreviewOptions.IsDisabled {
		t.Fatalf("request = %+v", received)
	}
}

func TestClientClassifiesRateLimitAndClampsRetry(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, `{"ok":false,"error_code":429,"description":"sensitive","parameters":{"retry_after":999999}}`)
	}))
	defer server.Close()

	err := testClient(server).Send(context.Background(), binding.TelegramTarget{ChatIdentity: "1"}, "summary")
	sendError, ok := AsSendError(err)
	if !ok || sendError.Failure != delivery.FailureRateLimited || sendError.Permanent || sendError.RetryAfter != maxRetryAfter {
		t.Fatalf("send error = %#v, %v", sendError, err)
	}
	if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestClientClassifiesPermanentAndTemporaryResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		status    int
		failure   delivery.Failure
		permanent bool
	}{
		{name: "bad request", status: http.StatusBadRequest, failure: delivery.FailureRejected, permanent: true},
		{name: "server", status: http.StatusBadGateway, failure: delivery.FailureUnavailable, permanent: false},
		{name: "timeout", status: http.StatusRequestTimeout, failure: delivery.FailureUnavailable, permanent: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, `{"ok":false,"description":"do-not-return"}`)
			}))
			defer server.Close()
			err := testClient(server).Send(context.Background(), binding.TelegramTarget{ChatIdentity: "1"}, "summary")
			sendError, ok := AsSendError(err)
			if !ok || sendError.Failure != test.failure || sendError.Permanent != test.permanent || strings.Contains(err.Error(), "do-not-return") {
				t.Fatalf("send error = %#v, %v", sendError, err)
			}
		})
	}
}

func TestClientDoesNotLeakTokenFromTransportError(t *testing.T) {
	t.Parallel()
	client := &Client{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("failed URL " + request.URL.String())
		})},
		BaseURL: "https://api.telegram.invalid", Token: func() (string, error) { return testToken, nil },
	}
	err := client.Send(context.Background(), binding.TelegramTarget{ChatIdentity: "1"}, "summary")
	sendError, ok := AsSendError(err)
	if !ok || sendError.Failure != delivery.FailureTransport || sendError.Permanent || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "telegram.invalid") {
		t.Fatalf("unsafe transport error: %#v, %v", sendError, err)
	}
}

func TestClientNeverFollowsRedirect(t *testing.T) {
	t.Parallel()
	var sinkCalled atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkCalled.Store(true)
	}))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, sink.URL+"/captured", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	err := testClient(server).Send(context.Background(), binding.TelegramTarget{ChatIdentity: "1"}, "summary")
	sendError, ok := AsSendError(err)
	if !ok || sendError.Permanent || sendError.Failure != delivery.FailureUnavailable || sinkCalled.Load() {
		t.Fatalf("redirect result = %#v, sinkCalled=%t", sendError, sinkCalled.Load())
	}
}

func TestClientRejectsInvalidInputBeforeRequest(t *testing.T) {
	t.Parallel()
	called := false
	client := &Client{
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("unexpected")
		})},
		Token: func() (string, error) { return testToken, nil },
	}
	for _, input := range []struct {
		target binding.TelegramTarget
		text   string
	}{
		{target: binding.TelegramTarget{ChatIdentity: "not-a-number"}, text: "summary"},
		{target: binding.TelegramTarget{ChatIdentity: "1", TopicIdentity: "-2"}, text: "summary"},
		{target: binding.TelegramTarget{ChatIdentity: "1"}, text: strings.Repeat("x", maxTextRunes+1)},
	} {
		err := client.Send(context.Background(), input.target, input.text)
		sendError, ok := AsSendError(err)
		if !ok || !sendError.Permanent {
			t.Fatalf("invalid input error = %#v, %v", sendError, err)
		}
	}
	if called {
		t.Fatal("transport called for invalid input")
	}
}

func TestFileTokenReloadsRotation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := FileToken(path)
	if token, err := provider(); err != nil || token != testToken {
		t.Fatalf("first token = %q, %v", token, err)
	}
	rotated := "654321:rotated_token"
	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := provider(); err != nil || token != rotated {
		t.Fatalf("rotated token = %q, %v", token, err)
	}
	if err := os.WriteFile(path, []byte("invalid/token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := provider(); err == nil || token != "" || strings.Contains(err.Error(), "invalid/token") {
		t.Fatalf("invalid token result = %q, %v", token, err)
	}
}

func testClient(server *httptest.Server) *Client {
	return &Client{
		HTTPClient: server.Client(), BaseURL: server.URL,
		Token: func() (string, error) { return testToken, nil },
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
