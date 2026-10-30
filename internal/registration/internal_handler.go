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
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
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
	"strconv"
	"strings"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	InternalRedemptionPath   = "/internal/v1/telegram/redemptions"
	internalBodyLimit        = 4096
	operationKeyHeader       = "X-Sentry-Operation-Key"
	operationSignatureHeader = "X-Sentry-Operation-Signature"
)

type TelegramRedeemer interface {
	RedeemCode(context.Context, string, binding.TelegramTarget) error
}

// InternalHandler accepts only the receiver-to-writer handoff. TLS termination must pass
// the verified peer chain through request.TLS; proxy headers and certificate CommonName
// are deliberately ignored.
type InternalHandler struct {
	Redeemer         TelegramRedeemer
	AllowedClientURI string
}

type redemptionRequest struct {
	Code    string `json:"code"`
	ChatID  string `json:"chatID"`
	TopicID string `json:"topicID,omitempty"`
}

func (h *InternalHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Pragma", "no-cache")
	if request.Method != http.MethodPost || request.URL.Path != InternalRedemptionPath {
		writeInternalError(response, http.StatusNotFound, "not found")
		return
	}
	if h.Redeemer == nil || h.AllowedClientURI == "" {
		writeInternalError(response, http.StatusServiceUnavailable, "registration writer unavailable")
		return
	}
	if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
		writeInternalError(response, http.StatusUnauthorized, "verified client certificate required")
		return
	}
	clientCertificate := verifiedClientCertificate(request.TLS.VerifiedChains, h.AllowedClientURI)
	if clientCertificate == nil {
		writeInternalError(response, http.StatusForbidden, "client identity denied")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeInternalError(response, http.StatusBadRequest, "invalid request")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, internalBodyLimit)
	body, err := io.ReadAll(request.Body)
	if err != nil || !verifyOperationSignature(request, body, clientCertificate) {
		writeInternalError(response, http.StatusUnauthorized, "signed operation required")
		return
	}
	input, err := decodeRedemptionRequest(bytes.NewReader(body))
	if err != nil {
		writeInternalError(response, http.StatusBadRequest, "invalid request")
		return
	}
	err = h.Redeemer.RedeemCode(request.Context(), input.Code, binding.TelegramTarget{
		ChatIdentity: input.ChatID, TopicIdentity: input.TopicID,
	})
	switch {
	case err == nil:
		response.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrPublishPending):
		// The target and pending outbox are durable. A retry worker resumes publication.
		response.WriteHeader(http.StatusAccepted)
	case errors.Is(err, ErrChallengeInvalid), errors.Is(err, ErrLiveVerification), errors.Is(err, ErrRegistrationRequired):
		writeInternalError(response, http.StatusConflict, "registration no longer valid")
	default:
		writeInternalError(response, http.StatusServiceUnavailable, "registration writer unavailable")
	}
}

func InternalServerTLSConfig(clientCAs *x509.CertPool) (*tls.Config, error) {
	if clientCAs == nil {
		return nil, errors.New("client CA pool is required")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		NextProtos: []string{"http/1.1"},
	}, nil
}

func verifiedClientCertificate(chains [][]*x509.Certificate, allowed string) *x509.Certificate {
	wanted, err := url.Parse(allowed)
	if err != nil || wanted.Scheme == "" || wanted.Host == "" {
		return nil
	}
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		for _, identity := range chain[0].URIs {
			if identity != nil && identity.String() == wanted.String() {
				return chain[0]
			}
		}
	}
	return nil
}

func verifyOperationSignature(request *http.Request, body []byte, certificate *x509.Certificate) bool {
	operationKey := request.Header.Get(operationKeyHeader)
	if !validOperationHash(operationKey) || certificate == nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Header.Get(operationSignatureHeader))
	if err != nil || len(signature) == 0 {
		return false
	}
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{"v1", request.Method, request.URL.Path, operationKey, base64.RawURLEncoding.EncodeToString(bodyHash[:])}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	switch publicKey := certificate.PublicKey.(type) {
	case ed25519.PublicKey:
		return ed25519.Verify(publicKey, digest[:], signature)
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(publicKey, digest[:], signature)
	case *rsa.PublicKey:
		return rsa.VerifyPSS(publicKey, crypto.SHA256, digest[:], signature, nil) == nil
	default:
		return false
	}
}

func decodeRedemptionRequest(body io.Reader) (redemptionRequest, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var input redemptionRequest
	if err := decoder.Decode(&input); err != nil {
		return redemptionRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return redemptionRequest{}, errors.New("request must contain one JSON object")
	}
	code, codeErr := base64.RawURLEncoding.DecodeString(input.Code)
	chatID, chatErr := strconv.ParseInt(input.ChatID, 10, 64)
	var topicErr error
	var topicID int64
	if input.TopicID != "" {
		topicID, topicErr = strconv.ParseInt(input.TopicID, 10, 64)
	}
	if codeErr != nil || len(code) != 32 || chatErr != nil || chatID == 0 || len(input.ChatID) > 128 || len(input.TopicID) > 128 || topicErr != nil || (input.TopicID != "" && topicID <= 0) {
		return redemptionRequest{}, errors.New("required field is missing or too long")
	}
	return input, nil
}

func writeInternalError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}
