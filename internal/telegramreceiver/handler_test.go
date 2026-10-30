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

package telegramreceiver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	testWebhookSecret = "telegram_Webhook-Secret"
	testCode          = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"
)

func TestHandlerAuthenticatesBeforeBodyAndForwardsOnlyCodeAndNumericTarget(t *testing.T) {
	t.Parallel()

	writer := &fakeWriter{}
	handler := &Handler{Secret: staticSecret(testWebhookSecret), Writer: writer, BotUsername: "alerts_bot"}
	body := `{"update_id":912,"message":{"message_thread_id":42,"chat":{"id":-100123},"text":"/register@alerts_bot ` + testCode + `","entities":[{"type":"bot_command","offset":0,"length":20}]}}`
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	request.Header.Set(TelegramSecretHeader, testWebhookSecret)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || writer.calls != 1 || writer.code != testCode || writer.target != (binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}) || len(writer.operationKey) != 64 {
		t.Fatalf("status=%d writer=%#v body=%s", response.Code, writer, response.Body.String())
	}

	reader := &countingReader{Reader: strings.NewReader(body)}
	request = httptest.NewRequest(http.MethodPost, WebhookPath, reader)
	request.Header.Set(TelegramSecretHeader, "wrong")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || reader.read != 0 || writer.calls != 1 || strings.Contains(response.Body.String(), testCode) {
		t.Fatalf("status=%d read=%d calls=%d body=%s", response.Code, reader.read, writer.calls, response.Body.String())
	}
}

func TestHandlerUsesTelegramUTF16EntityOffsetsAndIgnoresOtherUpdates(t *testing.T) {
	t.Parallel()

	writer := &fakeWriter{}
	handler := &Handler{Secret: staticSecret(testWebhookSecret), Writer: writer}
	for _, body := range []string{
		`{"update_id":1,"message":{"chat":{"id":123},"text":"hello"}}`,
		`{"update_id":2,"edited_message":{"chat":{"id":123},"text":"/register ` + testCode + `"}}`,
		`{"update_id":3,"message":{"chat":{"id":123},"text":"😀 /register ` + testCode + `","entities":[{"type":"bot_command","offset":3,"length":9}]}}`,
		`{"update_id":4,"message":{"chat":{"id":123},"text":"/register bad","entities":[{"type":"bot_command","offset":0,"length":9}]}}`,
	} {
		request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
		request.Header.Set(TelegramSecretHeader, testWebhookSecret)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if writer.calls != 0 {
		t.Fatalf("unexpected calls=%d", writer.calls)
	}
}

func TestHandlerRetriesTelegramOnlyForValidCommandWriterFailure(t *testing.T) {
	t.Parallel()

	writer := &fakeWriter{err: ErrWriterUnavailable}
	handler := &Handler{Secret: staticSecret(testWebhookSecret), Writer: writer}
	body := `{"update_id":5,"message":{"chat":{"id":123},"text":"/register ` + testCode + `","entities":[{"type":"bot_command","offset":0,"length":9}]}}`
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	request.Header.Set(TelegramSecretHeader, testWebhookSecret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), testCode) || strings.Contains(response.Body.String(), "123") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHandlerAcknowledgesTerminalWriterRejection(t *testing.T) {
	t.Parallel()

	writer := &fakeWriter{err: ErrWriterRejected}
	handler := &Handler{Secret: staticSecret(testWebhookSecret), Writer: writer}
	body := `{"update_id":6,"message":{"chat":{"id":123},"text":"/register ` + testCode + `","entities":[{"type":"bot_command","offset":0,"length":9}]}}`
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	request.Header.Set(TelegramSecretHeader, testWebhookSecret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || writer.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, writer.calls, response.Body.String())
	}
}

type staticSecret string

func (s staticSecret) Secret(context.Context) (string, error) { return string(s), nil }

type fakeWriter struct {
	calls        int
	code         string
	target       binding.TelegramTarget
	operationKey string
	err          error
}

func (f *fakeWriter) Redeem(_ context.Context, code string, target binding.TelegramTarget, operationKey string) error {
	f.calls++
	f.code = code
	f.target = target
	f.operationKey = operationKey
	return f.err
}

type countingReader struct {
	io.Reader
	read int
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.read += n
	return n, err
}
