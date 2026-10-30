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
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientUsesCustomCAAndRotatedToken(t *testing.T) {
	t.Parallel()

	var expectedToken atomic.Value
	expectedToken.Store("token-one")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer "+expectedToken.Load().(string); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if got := r.Header.Get("User-Agent"); got != "sentry-operator/test" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := mustClient(t, Config{
		BaseURL:   server.URL,
		Token:     " token-one\n",
		CABundle:  serverCA(t, server),
		UserAgent: "sentry-operator/test",
	})
	var result struct {
		OK bool `json:"ok"`
	}
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/api/0/", Operation: "discover"}, &result); err != nil {
		t.Fatalf("initial request: %v", err)
	}
	if !result.OK {
		t.Fatal("response was not decoded")
	}

	expectedToken.Store("token-two")
	client.UpdateToken("\ttoken-two\r\n")
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/api/0/", Operation: "discover"}, &result); err != nil {
		t.Fatalf("request after token rotation: %v", err)
	}
}

func TestClientReloadsCABundleWithoutRecreation(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := mustClient(t, Config{BaseURL: server.URL})
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "probe"}, &struct{}{}); !IsKind(err, ErrorTransport) {
		t.Fatalf("before CA rotation error = %v, want TransportError", err)
	}
	if err := client.UpdateCABundle([]byte("not a certificate")); err == nil {
		t.Fatal("invalid CA bundle was accepted")
	}
	if err := client.UpdateCABundle(serverCA(t, server)); err != nil {
		t.Fatalf("update CA bundle: %v", err)
	}
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "probe"}, &struct{}{}); err != nil {
		t.Fatalf("request after CA rotation: %v", err)
	}
}

func TestClientRejectsUnsafeOriginsAndRedirects(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{"http://sentry.example", "https://user:pass@sentry.example", "//sentry.example"} {
		if _, err := NewClient(Config{BaseURL: baseURL}); err == nil {
			t.Errorf("accepted unsafe base URL %q", baseURL)
		}
	}

	var leaked atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/capture", http.StatusFound)
	}))
	defer origin.Close()

	client := mustClient(t, Config{BaseURL: origin.URL, Token: "never-leak", CABundle: serverCA(t, origin)})
	_, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/redirect", Operation: "redirect"}, &struct{}{})
	if !IsKind(err, ErrorTransport) {
		t.Fatalf("redirect error = %v, want TransportError", err)
	}
	if leaked.Load() {
		t.Fatal("authorization was sent to a cross-origin redirect")
	}
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: target.URL + "/direct", Operation: "direct"}, &struct{}{}); !IsKind(err, ErrorInvalid) {
		t.Fatalf("cross-origin absolute path error = %v, want Invalid", err)
	}
	if strings.Contains(err.Error(), "never-leak") || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("safe error leaked sensitive data: %v", err)
	}
}

func TestClientRequiresExplicitInsecureTransportOptions(t *testing.T) {
	t.Parallel()

	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer httpServer.Close()
	if _, err := NewClient(Config{BaseURL: httpServer.URL}); err == nil {
		t.Fatal("plain HTTP was accepted without an explicit opt-in")
	}
	httpClient := mustClient(t, Config{BaseURL: httpServer.URL, AllowInsecureHTTP: true})
	if _, err := httpClient.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "http-probe"}, &struct {
		OK bool `json:"ok"`
	}{}); err != nil {
		t.Fatalf("explicit HTTP request failed: %v", err)
	}

	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer tlsServer.Close()
	strictClient := mustClient(t, Config{BaseURL: tlsServer.URL})
	if _, err := strictClient.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "strict-probe"}, &struct{}{}); !IsKind(err, ErrorTransport) {
		t.Fatalf("strict TLS error = %v, want TransportError", err)
	}
	insecureClient := mustClient(t, Config{BaseURL: tlsServer.URL, InsecureSkipTLSVerify: true})
	if _, err := insecureClient.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "skip-verify-probe"}, &struct{}{}); err != nil {
		t.Fatalf("explicit skip-verify request failed: %v", err)
	}

	if _, err := NewClient(Config{BaseURL: httpServer.URL, AllowInsecureHTTP: true, InsecureSkipTLSVerify: true}); err == nil {
		t.Fatal("skip TLS verification was accepted for HTTP")
	}
	if _, err := NewClient(Config{BaseURL: tlsServer.URL, InsecureSkipTLSVerify: true, CABundle: serverCA(t, tlsServer)}); err == nil {
		t.Fatal("skip TLS verification was accepted together with a CA bundle")
	}
}

func TestClientClassifiesStatusesWithoutResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/"), "%d", &status)
		w.Header().Set("X-Sentry-Request-ID", "request-123")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"super-secret-response-body","dsn":"https://public:secret@example/1"}`))
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, Token: "api-token", CABundle: serverCA(t, server)})

	cases := map[int]ErrorKind{
		http.StatusBadRequest:          ErrorInvalid,
		http.StatusUnauthorized:        ErrorUnauthorized,
		http.StatusForbidden:           ErrorPermissionDenied,
		http.StatusNotFound:            ErrorNotFound,
		http.StatusConflict:            ErrorConflict,
		http.StatusTooManyRequests:     ErrorRateLimited,
		http.StatusInternalServerError: ErrorServer,
	}
	for status, kind := range cases {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: fmt.Sprintf("/%d", status), Operation: "classify"}, &struct{}{})
			if !IsKind(err, kind) {
				t.Fatalf("error = %v, want kind %s", err, kind)
			}
			var apiErr *APIError
			if !AsAPIError(err, &apiErr) || apiErr.RequestID != "request-123" {
				t.Fatalf("request ID was not retained safely: %#v", apiErr)
			}
			for _, secret := range []string{"super-secret-response-body", "api-token", "public:secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

func TestClientRetriesOnlyExplicitlySafeRequests(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"found":true}`))
		}
	}))
	defer server.Close()

	var delays []time.Duration
	client := mustClient(t, Config{
		BaseURL:    server.URL,
		CABundle:   serverCA(t, server),
		MaxRetries: 2,
		MinBackoff: 20 * time.Millisecond,
		Jitter:     func(delay time.Duration) time.Duration { return delay },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	})
	var result map[string]bool
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", RetrySafe: true, Operation: "marker-probe"}, &result); err != nil {
		t.Fatalf("safe marker probe: %v", err)
	}
	if calls.Load() != 3 || len(delays) != 2 || delays[0] != 2*time.Second || delays[1] != 40*time.Millisecond {
		t.Fatalf("calls=%d delays=%v", calls.Load(), delays)
	}

	calls.Store(0)
	if _, err := client.DoJSON(context.Background(), Request{Method: http.MethodPost, Path: "/", Body: map[string]string{"name": "marker"}, Operation: "create-project"}, &struct{}{}); !IsKind(err, ErrorRateLimited) {
		t.Fatalf("mutating POST error = %v, want first 429", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("mutating POST was retried %d times", calls.Load())
	}
}

func TestTimedOutPOSTIsNotBlindlyRetried(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer server.Close()
	defer close(release)
	client := mustClient(t, Config{
		BaseURL:    server.URL,
		CABundle:   serverCA(t, server),
		Timeout:    500 * time.Millisecond,
		MaxRetries: 5,
	})
	_, err := client.DoJSON(context.Background(), Request{Method: http.MethodPost, Path: "/projects/", Body: map[string]string{"name": "managed"}, Operation: "create-project"}, &struct{}{})
	if !IsKind(err, ErrorTimeout) {
		t.Fatalf("error = %v, want Timeout", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("timed-out POST was attempted %d times", calls.Load())
	}
}

func TestResponseBodyTimeoutKeepsTimeoutClassification(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server), Timeout: 250 * time.Millisecond})
	_, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "slow-body"}, &struct{}{})
	if !IsKind(err, ErrorTimeout) {
		t.Fatalf("slow response body error = %v, want Timeout", err)
	}
}

func TestClientEnforcesConnectionConcurrency(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server), MaxConcurrent: 2})

	var wg sync.WaitGroup
	errCh := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "concurrency"}, &struct{}{})
			errCh <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("two requests did not enter handler")
		}
	}
	select {
	case <-entered:
		t.Fatal("third request bypassed connection concurrency limit")
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("request failed: %v", err)
		}
	}
}

func TestDecodeErrorIsRedacted(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"secret-token",broken`))
	}))
	defer server.Close()
	client := mustClient(t, Config{BaseURL: server.URL, CABundle: serverCA(t, server)})
	target := struct {
		Token string `json:"token"`
	}{Token: "unchanged"}
	_, err := client.DoJSON(context.Background(), Request{Method: http.MethodGet, Path: "/", Operation: "decode"}, &target)
	if !IsKind(err, ErrorDecode) || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "broken") {
		t.Fatalf("decode error is not safely redacted: %v", err)
	}
	if target.Token != "unchanged" {
		t.Fatalf("failed decode partially mutated target: %#v", target)
	}
}

func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func serverCA(t *testing.T, server *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}
