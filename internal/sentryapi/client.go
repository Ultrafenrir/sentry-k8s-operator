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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxResponseBytes = 4 << 20

// Config configures one connection-scoped Sentry API client.
type Config struct {
	BaseURL               string
	Token                 string
	CABundle              []byte
	AllowInsecureHTTP     bool
	InsecureSkipTLSVerify bool
	Timeout               time.Duration
	MaxConcurrent         int
	MaxRetries            int
	MinBackoff            time.Duration
	MaxBackoff            time.Duration
	UserAgent             string

	// Sleep and Jitter are injectable to make retry behavior deterministic in tests.
	Sleep  func(context.Context, time.Duration) error
	Jitter func(time.Duration) time.Duration
}

// Request describes a Sentry API request. Operation must be a static, non-sensitive label.
type Request struct {
	Method string
	Path   string
	Body   any
	// IfMatch carries an ETag returned by a fresh GET for conditional mutation.
	IfMatch   string
	RetrySafe bool
	Operation string
}

// Client is safe for concurrent use and supports credential and CA rotation.
type Client struct {
	baseURL               *url.URL
	httpClient            *http.Client
	transport             *rotatingTransport
	insecureSkipTLSVerify bool
	token                 atomic.Value
	semaphore             chan struct{}
	maxRetries            int
	minBackoff            time.Duration
	maxBackoff            time.Duration
	userAgent             string
	sleep                 func(context.Context, time.Duration) error
	jitter                func(time.Duration) time.Duration
}

type rotatingTransport struct {
	current atomic.Pointer[http.Transport]
}

func (t *rotatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.current.Load().RoundTrip(req)
}

// NewClient creates a client which is HTTPS-only unless plain HTTP is explicitly enabled.
func NewClient(cfg Config) (*Client, error) {
	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil || (baseURL.Scheme != "https" && baseURL.Scheme != "http") || baseURL.Host == "" || baseURL.User != nil {
		return nil, fmt.Errorf("base URL must be an absolute HTTP or HTTPS URL without userinfo")
	}
	if baseURL.Scheme == "http" && !cfg.AllowInsecureHTTP {
		return nil, fmt.Errorf("plain HTTP requires explicit allow-insecure-http configuration")
	}
	if baseURL.Scheme != "https" && cfg.InsecureSkipTLSVerify {
		return nil, fmt.Errorf("skip TLS verification requires an HTTPS base URL")
	}
	if cfg.InsecureSkipTLSVerify && len(cfg.CABundle) > 0 {
		return nil, fmt.Errorf("skip TLS verification and a CA bundle are mutually exclusive")
	}
	if baseURL.Scheme != "https" && len(cfg.CABundle) > 0 {
		return nil, fmt.Errorf("a CA bundle requires an HTTPS base URL")
	}
	baseURL.Fragment = ""
	baseURL.RawQuery = ""

	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.MaxRetries < 0 {
		return nil, fmt.Errorf("max retries must not be negative")
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 250 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		return nil, fmt.Errorf("max backoff must not be less than min backoff")
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepContext
	}
	if cfg.Jitter == nil {
		cfg.Jitter = cryptoJitter
	}

	transport, err := newHTTPTransport(cfg.CABundle, cfg.InsecureSkipTLSVerify)
	if err != nil {
		return nil, err
	}
	rotating := &rotatingTransport{}
	rotating.current.Store(transport)
	c := &Client{
		baseURL:               baseURL,
		transport:             rotating,
		insecureSkipTLSVerify: cfg.InsecureSkipTLSVerify,
		semaphore:             make(chan struct{}, cfg.MaxConcurrent),
		maxRetries:            cfg.MaxRetries,
		minBackoff:            cfg.MinBackoff,
		maxBackoff:            cfg.MaxBackoff,
		userAgent:             cfg.UserAgent,
		sleep:                 cfg.Sleep,
		jitter:                cfg.Jitter,
	}
	c.token.Store(strings.TrimSpace(cfg.Token))
	c.httpClient = &http.Client{
		Transport: rotating,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if !sameOrigin(baseURL, next.URL) {
				return errors.New("redirect rejected: target is outside the configured Sentry origin")
			}
			if len(via) >= 10 {
				return errors.New("redirect rejected: too many redirects")
			}
			return nil
		},
	}
	return c, nil
}

// UpdateToken atomically applies a rotated token to subsequent attempts.
func (c *Client) UpdateToken(token string) {
	c.token.Store(strings.TrimSpace(token))
}

// UpdateCABundle atomically applies a rotated CA bundle to subsequent requests.
func (c *Client) UpdateCABundle(caBundle []byte) error {
	if c.insecureSkipTLSVerify && len(caBundle) > 0 {
		return fmt.Errorf("skip TLS verification and a CA bundle are mutually exclusive")
	}
	next, err := newHTTPTransport(caBundle, c.insecureSkipTLSVerify)
	if err != nil {
		return err
	}
	previous := c.transport.current.Swap(next)
	if previous != nil {
		previous.CloseIdleConnections()
	}
	return nil
}

// DoJSON performs a request and decodes a successful JSON response into out.
func (c *Client) DoJSON(ctx context.Context, request Request, out any) (http.Header, error) {
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return nil, timeoutOrTransport(request.Operation, ctx.Err())
	}

	body, err := json.Marshal(request.Body)
	if err != nil {
		return nil, &APIError{Kind: ErrorInvalid, Operation: safeOperation(request.Operation)}
	}
	if request.Body == nil {
		body = nil
	}

	for attempt := 0; ; attempt++ {
		headers, retryAfter, retryable, err := c.doAttempt(ctx, request, body, out)
		if err == nil || !request.RetrySafe || !retryable || attempt >= c.maxRetries {
			return headers, err
		}
		delay := retryAfter
		if delay <= 0 {
			delay = c.backoff(attempt)
		}
		if err := c.sleep(ctx, c.jitter(delay)); err != nil {
			return nil, timeoutOrTransport(request.Operation, err)
		}
	}
}

func (c *Client) doAttempt(ctx context.Context, request Request, body []byte, out any) (http.Header, time.Duration, bool, error) {
	target, err := c.resolve(request.Path)
	if err != nil {
		return nil, 0, false, &APIError{Kind: ErrorInvalid, Operation: safeOperation(request.Operation)}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, target.String(), reader)
	if err != nil {
		return nil, 0, false, &APIError{Kind: ErrorInvalid, Operation: safeOperation(request.Operation)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if request.IfMatch != "" {
		req.Header.Set("If-Match", request.IfMatch)
	}
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if token, _ := c.token.Load().(string); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		apiErr := timeoutOrTransport(request.Operation, err)
		return nil, 0, apiErr.Kind == ErrorTimeout || apiErr.Kind == ErrorTransport, apiErr
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		kind := classifyStatus(response.StatusCode)
		return response.Header.Clone(), retryAfter, kind == ErrorRateLimited || kind == ErrorServer, &APIError{
			Kind:       kind,
			StatusCode: response.StatusCode,
			RetryAfter: retryAfter,
			RequestID:  response.Header.Get("X-Sentry-Request-ID"),
			Operation:  safeOperation(request.Operation),
		}
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return response.Header.Clone(), 0, false, nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		apiErr := timeoutOrTransport(request.Operation, err)
		return response.Header.Clone(), 0, apiErr.Kind == ErrorTimeout || apiErr.Kind == ErrorTransport, apiErr
	}
	if len(payload) > maxResponseBytes || decodeJSON(payload, out) != nil {
		return response.Header.Clone(), 0, false, &APIError{Kind: ErrorDecode, StatusCode: response.StatusCode, Operation: safeOperation(request.Operation)}
	}
	return response.Header.Clone(), 0, false, nil
}

func decodeJSON(payload []byte, out any) error {
	value := reflect.ValueOf(out)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("decode target must be a non-nil pointer")
	}
	temporary := reflect.New(value.Elem().Type())
	if err := json.Unmarshal(payload, temporary.Interface()); err != nil {
		return err
	}
	value.Elem().Set(temporary.Elem())
	return nil
}

func (c *Client) resolve(path string) (*url.URL, error) {
	reference, err := url.Parse(path)
	if err != nil || reference.User != nil {
		return nil, errors.New("invalid request path")
	}
	target := c.baseURL.ResolveReference(reference)
	if !sameOrigin(c.baseURL, target) {
		return nil, errors.New("request target is outside the configured Sentry origin")
	}
	return target, nil
}

func (c *Client) backoff(attempt int) time.Duration {
	delay := c.minBackoff
	for i := 0; i < attempt && delay < c.maxBackoff; i++ {
		if delay > c.maxBackoff/2 {
			return c.maxBackoff
		}
		delay *= 2
	}
	if delay > c.maxBackoff {
		return c.maxBackoff
	}
	return delay
}

func newHTTPTransport(caBundle []byte, insecureSkipTLSVerify bool) (*http.Transport, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if len(caBundle) > 0 && !roots.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("CA bundle contains no valid PEM certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, InsecureSkipVerify: insecureSkipTLSVerify} // #nosec G402 -- explicit per-connection development option
	return transport, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func timeoutOrTransport(operation string, err error) *APIError {
	kind := ErrorTransport
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorTimeout
	} else {
		var netError interface{ Timeout() bool }
		if errors.As(err, &netError) && netError.Timeout() {
			kind = ErrorTimeout
		}
	}
	return &APIError{Kind: kind, Operation: safeOperation(operation)}
}

func safeOperation(operation string) string {
	if operation == "" || len(operation) > 80 {
		return "request"
	}
	for _, char := range operation {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char)) {
			return "request"
		}
	}
	return operation
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cryptoJitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return delay
	}
	// Full jitter in [delay/2, delay] keeps a useful lower bound.
	half := delay / 2
	return half + time.Duration(binary.LittleEndian.Uint64(raw[:])%uint64(delay-half+1))
}
