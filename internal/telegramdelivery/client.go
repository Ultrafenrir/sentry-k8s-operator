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

// Package telegramdelivery contains the narrow Telegram sendMessage adapter.
// It returns only fixed error classes and never propagates response bodies,
// request URLs, bot tokens, chat identities or message content in errors.
package telegramdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

const (
	defaultBaseURL  = "https://api.telegram.org"
	maxResponseBody = 64 << 10
	maxTextRunes    = 4096
	maxTextBytes    = 16 << 10
	maxRetryAfter   = time.Hour
)

type TokenProvider func() (string, error)

// FileToken reloads a projected Secret on every operation so token rotation
// does not require restarting the worker.
func FileToken(path string) TokenProvider {
	return func() (string, error) {
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("telegram token is unavailable")
		}
		token := strings.TrimSpace(string(contents))
		if !validToken(token) {
			return "", errors.New("telegram token is unavailable")
		}
		return token, nil
	}
}

type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	Token      TokenProvider
}

// SendError is safe to log. It intentionally carries no wrapped cause.
type SendError struct {
	Failure    delivery.Failure
	RetryAfter time.Duration
	Permanent  bool
}

func (e *SendError) Error() string { return "telegram delivery failed" }

func (c *Client) Send(ctx context.Context, target binding.TelegramTarget, text string) error {
	if c == nil || c.HTTPClient == nil || c.Token == nil || !validText(text) {
		return safeError(delivery.FailureRejected, 0, true)
	}
	chatID, topicID, ok := parseTarget(target)
	if !ok {
		return safeError(delivery.FailureInvalidRoute, 0, true)
	}
	token, err := c.Token()
	if err != nil || !validToken(token) {
		return safeError(delivery.FailureUnavailable, 0, false)
	}
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return safeError(delivery.FailureUnavailable, 0, false)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/bot" + token + "/sendMessage"
	payload := sendMessageRequest{
		ChatID: chatID, MessageThreadID: topicID, Text: text,
		LinkPreviewOptions: linkPreviewOptions{IsDisabled: true},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return safeError(delivery.FailureRejected, 0, true)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return safeError(delivery.FailureUnavailable, 0, false)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	httpClient := *c.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(request)
	if err != nil {
		return safeError(delivery.FailureTransport, 0, false)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil || len(contents) > maxResponseBody {
		return safeError(delivery.FailureUnavailable, 0, false)
	}
	var result apiResponse
	decoded := json.Unmarshal(contents, &result) == nil
	if response.StatusCode >= 200 && response.StatusCode < 300 && decoded && result.OK {
		return nil
	}
	if response.StatusCode == http.StatusTooManyRequests || (decoded && result.ErrorCode == http.StatusTooManyRequests) {
		return safeError(delivery.FailureRateLimited, retryDuration(result.Parameters.RetryAfter), false)
	}
	if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
		return safeError(delivery.FailureUnavailable, 0, false)
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return safeError(delivery.FailureRejected, 0, true)
	}
	return safeError(delivery.FailureUnavailable, 0, false)
}

type sendMessageRequest struct {
	ChatID             int64              `json:"chat_id"`
	MessageThreadID    *int64             `json:"message_thread_id,omitempty"`
	Text               string             `json:"text"`
	LinkPreviewOptions linkPreviewOptions `json:"link_preview_options"`
}

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

type apiResponse struct {
	OK         bool `json:"ok"`
	ErrorCode  int  `json:"error_code,omitempty"`
	Parameters struct {
		RetryAfter int64 `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

func parseTarget(target binding.TelegramTarget) (int64, *int64, bool) {
	chatID, err := strconv.ParseInt(target.ChatIdentity, 10, 64)
	if err != nil || chatID == 0 {
		return 0, nil, false
	}
	if target.TopicIdentity == "" {
		return chatID, nil, true
	}
	topicID, err := strconv.ParseInt(target.TopicIdentity, 10, 64)
	if err != nil || topicID <= 0 {
		return 0, nil, false
	}
	return chatID, &topicID, true
}

func validText(text string) bool {
	return text != "" && utf8.ValidString(text) && utf8.RuneCountInString(text) <= maxTextRunes && len(text) <= maxTextBytes
}

func validToken(token string) bool {
	separator := strings.IndexByte(token, ':')
	if len(token) < 3 || len(token) > 256 || strings.Count(token, ":") != 1 || separator <= 0 || separator == len(token)-1 {
		return false
	}
	for _, character := range token {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == ':' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func retryDuration(seconds int64) time.Duration {
	if seconds <= 0 {
		return time.Second
	}
	if seconds > int64(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(seconds) * time.Second
}

func safeError(failure delivery.Failure, retryAfter time.Duration, permanent bool) error {
	return &SendError{Failure: failure, RetryAfter: retryAfter, Permanent: permanent}
}

func AsSendError(err error) (*SendError, bool) {
	var sendError *SendError
	if !errors.As(err, &sendError) {
		return nil, false
	}
	return sendError, true
}
