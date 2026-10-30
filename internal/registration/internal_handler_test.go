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

package registration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const receiverURI = "spiffe://cluster.local/ns/sentry-kubernetes-operator-system/sa/telegram-receiver"
const validRegistrationCode = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"

func TestInternalHandlerAcceptsOnlyVerifiedExactURIAndForwardsPrivateTarget(t *testing.T) {
	t.Parallel()

	redeemer := &fakeRedeemer{}
	handler := &InternalHandler{Redeemer: redeemer, AllowedClientURI: receiverURI}
	request := internalRequest(t, `{"code":"`+validRegistrationCode+`","chatID":"-100123","topicID":"42"}`, receiverURI)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if redeemer.calls != 1 || redeemer.code != validRegistrationCode || redeemer.target.ChatIdentity != "-100123" || redeemer.target.TopicIdentity != "42" {
		t.Fatalf("redeemer=%#v", redeemer)
	}
}

func TestInternalHandlerRejectsMissingOrWrongClientIdentityWithoutReadingBody(t *testing.T) {
	t.Parallel()

	for name, tlsState := range map[string]struct {
		state *tls.ConnectionState
		code  int
	}{
		"missing": {state: nil, code: http.StatusUnauthorized},
		"wrong":   {state: verifiedTLSState(t, "spiffe://cluster.local/ns/other/sa/receiver"), code: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			redeemer := &fakeRedeemer{}
			handler := &InternalHandler{Redeemer: redeemer, AllowedClientURI: receiverURI}
			request := httptest.NewRequest(http.MethodPost, InternalRedemptionPath, strings.NewReader(`{"code":"must-not-leak"}`))
			request.TLS = tlsState.state
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tlsState.code || redeemer.calls != 0 || strings.Contains(response.Body.String(), "must-not-leak") {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, redeemer.calls, response.Body.String())
			}
		})
	}
}

func TestInternalHandlerKeepsDurablePendingPublishOpaque(t *testing.T) {
	t.Parallel()

	redeemer := &fakeRedeemer{err: ErrPublishPending}
	handler := &InternalHandler{Redeemer: redeemer, AllowedClientURI: receiverURI}
	request := internalRequest(t, `{"code":"`+validRegistrationCode+`","chatID":"-100123"}`, receiverURI)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), validRegistrationCode) || strings.Contains(response.Body.String(), "-100123") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInternalHandlerRejectsInvalidBodySignatureBeforeRedemption(t *testing.T) {
	t.Parallel()

	redeemer := &fakeRedeemer{}
	handler := &InternalHandler{Redeemer: redeemer, AllowedClientURI: receiverURI}
	request := internalRequest(t, `{"code":"`+validRegistrationCode+`","chatID":"-100123"}`, receiverURI)
	request.Header.Set(operationSignatureHeader, base64.RawURLEncoding.EncodeToString([]byte("invalid-signature")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || redeemer.calls != 0 || strings.Contains(response.Body.String(), validRegistrationCode) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, redeemer.calls, response.Body.String())
	}
}

func TestInternalHandlerRejectsUnknownFieldsAndStaleChallengesWithoutLeaks(t *testing.T) {
	t.Parallel()

	redeemer := &fakeRedeemer{err: ErrChallengeInvalid}
	handler := &InternalHandler{Redeemer: redeemer, AllowedClientURI: receiverURI}
	request := internalRequest(t, `{"code":"`+validRegistrationCode+`","chatID":"-100123","unexpected":true}`, receiverURI)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || redeemer.calls != 0 || strings.Contains(response.Body.String(), validRegistrationCode) {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, redeemer.calls, response.Body.String())
	}

	request = internalRequest(t, `{"code":"`+validRegistrationCode+`","chatID":"-100123"}`, receiverURI)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), validRegistrationCode) || strings.Contains(response.Body.String(), "-100123") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestInternalServerTLSConfigRequiresTLS13AndVerifiedClientCertificate(t *testing.T) {
	t.Parallel()

	if _, err := InternalServerTLSConfig(nil); err == nil {
		t.Fatal("nil client CA accepted")
	}
	pool := x509.NewCertPool()
	config, err := InternalServerTLSConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS13 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs != pool || len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
		t.Fatalf("TLS config=%#v", config)
	}
}

func internalRequest(t *testing.T, body, identity string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, InternalRedemptionPath, strings.NewReader(body))
	state, privateKey := signedTLSState(t, identity)
	request.TLS = state
	request.Header.Set("Content-Type", "application/json")
	operationKey := strings.Repeat("f", 64)
	request.Header.Set(operationKeyHeader, operationKey)
	bodyHash := sha256.Sum256([]byte(body))
	canonical := strings.Join([]string{"v1", request.Method, request.URL.Path, operationKey, base64.RawURLEncoding.EncodeToString(bodyHash[:])}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(operationSignatureHeader, base64.RawURLEncoding.EncodeToString(signature))
	return request
}

func verifiedTLSState(t *testing.T, identity string) *tls.ConnectionState {
	t.Helper()
	state, _ := signedTLSState(t, identity)
	return state
}

func signedTLSState(t *testing.T, identity string) (*tls.ConnectionState, *ecdsa.PrivateKey) {
	t.Helper()
	parsed, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{parsed}, PublicKey: &privateKey.PublicKey}
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}, privateKey
}

type fakeRedeemer struct {
	calls  int
	code   string
	target binding.TelegramTarget
	err    error
}

func (f *fakeRedeemer) RedeemCode(_ context.Context, code string, target binding.TelegramTarget) error {
	f.calls++
	f.code = code
	f.target = target
	return f.err
}

var _ TelegramRedeemer = (*fakeRedeemer)(nil)
