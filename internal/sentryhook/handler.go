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

// Package sentryhook verifies Sentry project service-hook requests before any
// payload parsing or routing. It intentionally exposes only an opaque dedupe
// key and a locally configured source binding to downstream code.
package sentryhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader = "X-ServiceHook-Signature"
	TimestampHeader = "X-ServiceHook-Timestamp"
	GUIDHeader      = "X-ServiceHook-GUID"
	defaultMaxBody  = int64(64 << 10)
)

var ErrSink = errors.New("signed Sentry event was not durably accepted")

type SecretSource func(context.Context) ([]byte, error)
type SecretSetSource func(context.Context) ([][]byte, error)

// Envelope contains no raw Sentry event, external ID, URL, title, user data, or
// tag. DedupeKey is a one-way digest of the local source and authenticated event ID.
type Envelope struct {
	DedupeKey        string
	SourceBindingKey string
}

// Sink must be idempotent by Envelope.DedupeKey and return only after durable
// commit. A retry after a sink error is expected and must be safe.
type Sink interface {
	Accept(context.Context, Envelope) error
}

type Handler struct {
	Secret              SecretSource
	Secrets             SecretSetSource
	Sink                Sink
	Path                string
	SourceBindingKey    string
	ExpectedProjectSlug string
	MaxBodyBytes        int64
	MaxClockSkew        time.Duration
	Now                 func() time.Time
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path != h.Path || request.URL.RawQuery != "" {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || request.Header.Get("Content-Encoding") != "" {
		response.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if !h.valid() {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	maxBody := h.MaxBodyBytes
	if maxBody == 0 {
		maxBody = defaultMaxBody
	}
	if request.ContentLength > maxBody {
		response.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBody+1))
	if err != nil || int64(len(body)) > maxBody {
		response.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	secrets, err := h.loadSecrets(request.Context())
	if err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	signature, signatureOK := exactHeader(request.Header, SignatureHeader)
	timestampValue, timestampOK := exactHeader(request.Header, TimestampHeader)
	guid, guidOK := exactHeader(request.Header, GUIDHeader)
	if !signatureOK || !timestampOK || !guidOK || !validHeaderValue(guid, 256) || !validHeaderValue(timestampValue, 32) || !validSignature(signature) {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	timestamp, err := strconv.ParseInt(timestampValue, 10, 64)
	if err != nil || !withinSkew(h.now(), time.Unix(timestamp, 0), h.MaxClockSkew) || !validAnyMAC(body, secrets, signature) {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	var payload struct {
		Project struct {
			Slug string `json:"slug"`
		} `json:"project"`
		Event struct {
			EventID string `json:"eventID"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Project.Slug != h.ExpectedProjectSlug || !validHeaderValue(payload.Event.EventID, 256) {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	envelope := Envelope{
		DedupeKey:        dedupeKey(h.SourceBindingKey, payload.Event.EventID),
		SourceBindingKey: h.SourceBindingKey,
	}
	if err := h.Sink.Accept(request.Context(), envelope); err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (h *Handler) valid() bool {
	if h == nil {
		return false
	}
	maxBody := h.MaxBodyBytes
	if maxBody == 0 {
		maxBody = defaultMaxBody
	}
	return (h.Secret != nil) != (h.Secrets != nil) && h.Sink != nil && h.Path != "" && strings.HasPrefix(h.Path, "/") &&
		h.SourceBindingKey != "" && len(h.SourceBindingKey) <= 256 && validSlug(h.ExpectedProjectSlug) &&
		maxBody > 0 && maxBody <= 1<<20 && h.MaxClockSkew > 0 && h.MaxClockSkew <= time.Hour
}

func (h *Handler) loadSecrets(ctx context.Context) ([][]byte, error) {
	var secrets [][]byte
	var err error
	if h.Secrets != nil {
		secrets, err = h.Secrets(ctx)
	} else if h.Secret != nil {
		var secret []byte
		secret, err = h.Secret(ctx)
		secrets = [][]byte{secret}
	}
	if err != nil || len(secrets) == 0 || len(secrets) > 2 {
		return nil, errors.New("invalid service-hook secret set")
	}
	result := make([][]byte, len(secrets))
	seen := map[string]struct{}{}
	for index, secret := range secrets {
		if len(secret) < 16 || len(secret) > 4096 {
			return nil, errors.New("invalid service-hook secret set")
		}
		key := string(secret)
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("invalid service-hook secret set")
		}
		seen[key] = struct{}{}
		result[index] = append([]byte(nil), secret...)
	}
	return result, nil
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func validAnyMAC(body []byte, secrets [][]byte, encoded string) bool {
	provided, err := hex.DecodeString(encoded)
	if err != nil || len(provided) != sha256.Size || encoded != hex.EncodeToString(provided) {
		return false
	}
	matched := false
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(body)
		matched = hmac.Equal(mac.Sum(nil), provided) || matched
	}
	return matched
}

func exactHeader(headers http.Header, name string) (string, bool) {
	values := headers.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1
}

func validSignature(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}

func validHeaderValue(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validSlug(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || (character == '-' && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

func withinSkew(now, timestamp time.Time, maximum time.Duration) bool {
	delta := now.Sub(timestamp)
	return delta >= -maximum && delta <= maximum
}

func dedupeKey(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
