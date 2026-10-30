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
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const (
	InternalDeliveryPath  = "/internal/v1/sentry-hooks/deliveries"
	internalEnvelopeLimit = 1024
	internalOperationKey  = "X-Sentry-Operation-Key"
	internalOperationSig  = "X-Sentry-Operation-Signature"
)

// InternalProducerHandler is the only Kubernetes-writing side of hook ingress. The
// public verifier authenticates the Sentry HMAC, then hands off this closed envelope over
// mTLS. Proxy identity headers and certificate CommonName are ignored.
type InternalProducerHandler struct {
	Sink             Sink
	AllowedClientURI string
}

func (h *InternalProducerHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost || request.URL.Path != InternalDeliveryPath || request.URL.RawQuery != "" {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	if h == nil || h.Sink == nil || h.AllowedClientURI == "" {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	certificate, status := internalClientCertificate(request.TLS, h.AllowedClientURI)
	if certificate == nil {
		response.WriteHeader(status)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || request.Header.Get("Content-Encoding") != "" {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, internalEnvelopeLimit)
	body, err := io.ReadAll(request.Body)
	if err != nil || !verifyInternalEnvelope(request, body, certificate) {
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF || !canonicalHash(envelope.DedupeKey) ||
		!canonicalHash(envelope.SourceBindingKey) || request.Header.Get(internalOperationKey) != envelope.DedupeKey {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := h.Sink.Accept(request.Context(), envelope); err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

type ProducerClient struct {
	URL            string
	HTTPClient     *http.Client
	Signer         crypto.Signer
	SignerProvider func() (crypto.Signer, error)
}

func (c *ProducerClient) Accept(ctx context.Context, envelope Envelope) error {
	if c == nil || c.HTTPClient == nil || !canonicalHash(envelope.DedupeKey) || !canonicalHash(envelope.SourceBindingKey) {
		return ErrSink
	}
	parsed, err := url.Parse(c.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != InternalDeliveryPath || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrSink
	}
	signer := c.Signer
	if c.SignerProvider != nil {
		signer, err = c.SignerProvider()
		if err != nil {
			return ErrSink
		}
	}
	if signer == nil {
		return ErrSink
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return ErrSink
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return ErrSink
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(internalOperationKey, envelope.DedupeKey)
	signature, err := signInternalEnvelope(signer, request.Method, request.URL.Path, envelope.DedupeKey, body)
	if err != nil {
		return ErrSink
	}
	request.Header.Set(internalOperationSig, signature)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return ErrSink
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode != http.StatusNoContent {
		return ErrSink
	}
	return nil
}

func InternalProducerTLSConfig(clientCAs *x509.CertPool) (*tls.Config, error) {
	if clientCAs == nil {
		return nil, errors.New("client CA pool is required")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs, NextProtos: []string{"http/1.1"}}, nil
}

func internalClientCertificate(state *tls.ConnectionState, allowed string) (*x509.Certificate, int) {
	if state == nil || len(state.VerifiedChains) == 0 {
		return nil, http.StatusUnauthorized
	}
	wanted, err := url.Parse(allowed)
	if err != nil || wanted.Scheme == "" || wanted.Host == "" {
		return nil, http.StatusForbidden
	}
	for _, chain := range state.VerifiedChains {
		if len(chain) == 0 {
			continue
		}
		for _, identity := range chain[0].URIs {
			if identity != nil && identity.String() == wanted.String() {
				return chain[0], 0
			}
		}
	}
	return nil, http.StatusForbidden
}

func verifyInternalEnvelope(request *http.Request, body []byte, certificate *x509.Certificate) bool {
	operationKey := request.Header.Get(internalOperationKey)
	if !canonicalHash(operationKey) || certificate == nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Header.Get(internalOperationSig))
	if err != nil || len(signature) == 0 {
		return false
	}
	digest := internalEnvelopeDigest(request.Method, request.URL.Path, operationKey, body)
	switch publicKey := certificate.PublicKey.(type) {
	case ed25519.PublicKey:
		return ed25519.Verify(publicKey, digest, signature)
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(publicKey, digest, signature)
	case *rsa.PublicKey:
		return rsa.VerifyPSS(publicKey, crypto.SHA256, digest, signature, nil) == nil
	default:
		return false
	}
}

func signInternalEnvelope(signer crypto.Signer, method, path, operationKey string, body []byte) (string, error) {
	digest := internalEnvelopeDigest(method, path, operationKey, body)
	var options crypto.SignerOpts = crypto.SHA256
	if _, ok := signer.Public().(ed25519.PublicKey); ok {
		options = crypto.Hash(0)
	}
	if _, ok := signer.Public().(*rsa.PublicKey); ok {
		options = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	}
	signature, err := signer.Sign(rand.Reader, digest, options)
	if err != nil || len(signature) == 0 {
		return "", ErrSink
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func internalEnvelopeDigest(method, path, operationKey string, body []byte) []byte {
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{"v1", method, path, operationKey, base64.RawURLEncoding.EncodeToString(bodyHash[:])}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return digest[:]
}

var _ Sink = (*ProducerClient)(nil)
