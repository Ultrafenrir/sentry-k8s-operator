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
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	operationKeyHeader       = "X-Sentry-Operation-Key"
	operationSignatureHeader = "X-Sentry-Operation-Signature"
)

type WriterClient struct {
	URL            string
	HTTPClient     *http.Client
	Signer         crypto.Signer
	SignerProvider func() (crypto.Signer, error)
}

func (c *WriterClient) Redeem(ctx context.Context, code string, target binding.TelegramTarget, operationKey string) error {
	if c == nil || c.URL == "" || c.HTTPClient == nil || len(operationKey) != 64 {
		return ErrWriterUnavailable
	}
	signer := c.Signer
	if c.SignerProvider != nil {
		var err error
		signer, err = c.SignerProvider()
		if err != nil {
			return ErrWriterUnavailable
		}
	}
	if signer == nil {
		return ErrWriterUnavailable
	}
	body, err := json.Marshal(struct {
		Code    string `json:"code"`
		ChatID  string `json:"chatID"`
		TopicID string `json:"topicID,omitempty"`
	}{Code: code, ChatID: target.ChatIdentity, TopicID: target.TopicIdentity})
	if err != nil {
		return ErrWriterUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return ErrWriterUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(operationKeyHeader, operationKey)
	signature, err := signOperation(signer, request.Method, request.URL.Path, operationKey, body)
	if err != nil {
		return ErrWriterUnavailable
	}
	request.Header.Set(operationSignatureHeader, signature)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return ErrWriterUnavailable
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode == http.StatusAccepted {
		return ErrWriterPending
	}
	if response.StatusCode == http.StatusConflict {
		return ErrWriterRejected
	}
	return ErrWriterUnavailable
}

func signOperation(signer crypto.Signer, method, path, operationKey string, body []byte) (string, error) {
	bodyHash := sha256.Sum256(body)
	canonical := strings.Join([]string{"v1", method, path, operationKey, base64.RawURLEncoding.EncodeToString(bodyHash[:])}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	var opts crypto.SignerOpts = crypto.SHA256
	switch signer.Public().(type) {
	case ed25519.PublicKey:
		opts = crypto.Hash(0)
	case *rsa.PublicKey:
		opts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	}
	signature, err := signer.Sign(rand.Reader, digest[:], opts)
	if err != nil {
		return "", fmt.Errorf("sign operation: %w", err)
	}
	if len(signature) == 0 {
		return "", errors.New("empty operation signature")
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

var _ Writer = (*WriterClient)(nil)
