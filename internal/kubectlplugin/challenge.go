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

// Package kubectlplugin implements the client side of the Kubernetes-authenticated
// Telegram registration flow. It never persists or logs the returned one-time code.
package kubectlplugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

var ErrChallengeUnavailable = errors.New("registration challenge unavailable")

type Client struct {
	HTTPClient *http.Client
	Endpoint   string
}

type Request struct {
	Namespace   string `json:"namespace"`
	Project     string `json:"project"`
	Destination string `json:"destination"`
}

type Response struct {
	Command   string      `json:"command"`
	ExpiresAt metav1.Time `json:"expiresAt"`
}

func (c *Client) Issue(ctx context.Context, input Request) (Response, error) {
	endpoint, err := url.Parse(c.Endpoint)
	if c == nil || c.HTTPClient == nil || err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || input.Namespace == "" || input.Project == "" || input.Destination == "" {
		return Response{}, ErrChallengeUnavailable
	}
	endpoint.Path = registration.ChallengePath
	body, err := json.Marshal(input)
	if err != nil {
		return Response{}, ErrChallengeUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, ErrChallengeUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return Response{}, ErrChallengeUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Response{}, ErrChallengeUnavailable
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	var result Response
	if err := decoder.Decode(&result); err != nil {
		return Response{}, ErrChallengeUnavailable
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Response{}, ErrChallengeUnavailable
	}
	code := strings.TrimPrefix(result.Command, "/register ")
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(code)
	if result.Command == code || decodeErr != nil || len(decoded) != 32 || result.ExpiresAt.IsZero() {
		return Response{}, ErrChallengeUnavailable
	}
	return result, nil
}

func NewHTTPClient(config *http.Client, timeout time.Duration) (*http.Client, error) {
	if config == nil || config.Transport == nil || timeout <= 0 {
		return nil, ErrChallengeUnavailable
	}
	return &http.Client{
		Transport: config.Transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("registration endpoint redirects are disabled")
		},
	}, nil
}
