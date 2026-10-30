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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

func TestWriterClientSignsExactBodyAndOmitsProjectIdentifiers(t *testing.T) {
	t.Parallel()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	operationKey := strings.Repeat("a", 64)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		if strings.Contains(string(body), "project") || strings.Contains(string(body), "destination") {
			t.Errorf("sensitive routing identity in body: %s", body)
		}
		bodyHash := sha256.Sum256(body)
		canonical := strings.Join([]string{"v1", request.Method, request.URL.Path, operationKey, base64.RawURLEncoding.EncodeToString(bodyHash[:])}, "\n")
		digest := sha256.Sum256([]byte(canonical))
		signature, decodeErr := base64.RawURLEncoding.DecodeString(request.Header.Get(operationSignatureHeader))
		if decodeErr != nil || !ecdsa.VerifyASN1(&privateKey.PublicKey, digest[:], signature) {
			t.Errorf("invalid signature: %v", decodeErr)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	client := &WriterClient{
		URL:        "https://registration-writer.example/internal/v1/telegram/redemptions",
		HTTPClient: &http.Client{Transport: transport}, Signer: privateKey,
	}
	err = client.Redeem(context.Background(), testCode, binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}, operationKey)
	if err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
