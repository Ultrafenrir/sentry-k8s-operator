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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const hookReceiverURI = "spiffe://cluster.local/ns/sentry-kubernetes-operator-system/sa/sentry-hook-receiver"

func TestInternalProducerHandlerAcceptsOnlySignedVerifiedClosedEnvelope(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	handler := &InternalProducerHandler{Sink: sink, AllowedClientURI: hookReceiverURI}
	envelope := Envelope{DedupeKey: strings.Repeat("a", 64), SourceBindingKey: strings.Repeat("b", 64)}
	request := signedInternalRequest(t, envelope, hookReceiverURI)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || len(sink.envelopes) != 1 || sink.envelopes[0] != envelope {
		t.Fatalf("status=%d body=%q envelopes=%#v", response.Code, response.Body.String(), sink.envelopes)
	}

	request = signedInternalRequest(t, envelope, hookReceiverURI)
	request.Header.Set(internalOperationSig, "tampered")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || len(sink.envelopes) != 1 {
		t.Fatalf("tampered status=%d envelopes=%d", response.Code, len(sink.envelopes))
	}
}

func TestInternalProducerHandlerRejectsIdentityShapeAndSinkFailureWithoutLeaks(t *testing.T) {
	t.Parallel()
	sentinel := "SENTINEL-MUST-NOT-LEAK"
	envelope := Envelope{DedupeKey: strings.Repeat("c", 64), SourceBindingKey: strings.Repeat("d", 64)}
	for name, configure := range map[string]func(*http.Request){
		"missing-tls": func(request *http.Request) { request.TLS = nil },
		"wrong-uri": func(request *http.Request) {
			request.TLS, _ = internalTLSState(t, "spiffe://cluster.local/ns/other/sa/receiver")
		},
		"unknown-field": func(request *http.Request) {
			body := `{"dedupeKey":"` + strings.Repeat("c", 64) + `","sourceBindingKey":"` + strings.Repeat("d", 64) + `","raw":"` + sentinel + `"}`
			request.Body = io.NopCloser(strings.NewReader(body))
			_, signer := internalTLSState(t, hookReceiverURI)
			signature, err := signInternalEnvelope(signer, request.Method, request.URL.Path, envelope.DedupeKey, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{
				URIs: request.TLS.VerifiedChains[0][0].URIs, PublicKey: signer.Public(),
			}}}}
			request.Header.Set(internalOperationSig, signature)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &recordingSink{}
			request := signedInternalRequest(t, envelope, hookReceiverURI)
			configure(request)
			response := httptest.NewRecorder()
			(&InternalProducerHandler{Sink: sink, AllowedClientURI: hookReceiverURI}).ServeHTTP(response, request)
			if response.Code == http.StatusNoContent || len(sink.envelopes) != 0 || strings.Contains(response.Body.String(), sentinel) {
				t.Fatalf("status=%d calls=%d body=%q", response.Code, len(sink.envelopes), response.Body.String())
			}
		})
	}
	sink := &recordingSink{err: ErrProducer}
	request := signedInternalRequest(t, envelope, hookReceiverURI)
	response := httptest.NewRecorder()
	(&InternalProducerHandler{Sink: sink, AllowedClientURI: hookReceiverURI}).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), envelope.DedupeKey) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestProducerClientSignsExactEnvelopeAndFailsClosed(t *testing.T) {
	t.Parallel()
	state, signer := internalTLSState(t, hookReceiverURI)
	certificate := state.VerifiedChains[0][0]
	envelope := Envelope{DedupeKey: strings.Repeat("e", 64), SourceBindingKey: strings.Repeat("f", 64)}
	calls := 0
	client := &ProducerClient{
		URL: "https://producer.internal" + InternalDeliveryPath,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(request.Body)
			if err != nil || !verifyInternalEnvelope(request, body, certificate) || request.Header.Get(internalOperationKey) != envelope.DedupeKey {
				t.Fatalf("unsigned request: error=%v headers=%v", err, request.Header)
			}
			return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})},
		Signer: signer,
	}
	if err := client.Accept(context.Background(), envelope); err != nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
	client.URL = "http://producer.internal" + InternalDeliveryPath
	if err := client.Accept(context.Background(), envelope); err != ErrSink || calls != 1 {
		t.Fatalf("insecure URL error=%v calls=%d", err, calls)
	}
}

func TestInternalProducerTLSConfigRequiresTLS13ClientVerification(t *testing.T) {
	t.Parallel()
	if _, err := InternalProducerTLSConfig(nil); err == nil {
		t.Fatal("nil client CA accepted")
	}
	pool := x509.NewCertPool()
	config, err := InternalProducerTLSConfig(pool)
	if err != nil || config.MinVersion != tls.VersionTLS13 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs != pool {
		t.Fatalf("config=%#v error=%v", config, err)
	}
}

func signedInternalRequest(t *testing.T, envelope Envelope, identity string) *http.Request {
	t.Helper()
	body := `{"dedupeKey":"` + envelope.DedupeKey + `","sourceBindingKey":"` + envelope.SourceBindingKey + `"}`
	request := httptest.NewRequest(http.MethodPost, InternalDeliveryPath, strings.NewReader(body))
	state, signer := internalTLSState(t, identity)
	request.TLS = state
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(internalOperationKey, envelope.DedupeKey)
	signature, err := signInternalEnvelope(signer, request.Method, request.URL.Path, envelope.DedupeKey, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(internalOperationSig, signature)
	return request
}

func internalTLSState(t *testing.T, identity string) (*tls.ConnectionState, *ecdsa.PrivateKey) {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
