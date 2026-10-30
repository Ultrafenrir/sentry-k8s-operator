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

package kubectlplugin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

const challengeCode = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"

func TestClientRequestsExactChallengePathAndReturnsOnlyValidatedCommand(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != registration.ChallengePath || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request=%s %s headers=%v", request.Method, request.URL, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"namespace":"payments","project":"checkout","destination":"telegram"}` {
			t.Errorf("body=%s", body)
		}
		return &http.Response{
			StatusCode: http.StatusCreated, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"command":"/register ` + challengeCode + `","expiresAt":"2026-10-30T00:20:00Z"}`)),
		}, nil
	})
	client := &Client{HTTPClient: &http.Client{Transport: transport}, Endpoint: "https://registration.example"}
	result, err := client.Issue(context.Background(), Request{Namespace: "payments", Project: "checkout", Destination: "telegram"})
	if err != nil || result.Command != "/register "+challengeCode {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestClientRejectsRedirectsErrorsAndMalformedCommands(t *testing.T) {
	t.Parallel()

	for name, response := range map[string]*http.Response{
		"status":  {StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`))},
		"command": {StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"command":"/register short","expiresAt":"2026-10-30T00:20:00Z"}`))},
	} {
		t.Run(name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil })
			client := &Client{HTTPClient: &http.Client{Transport: transport}, Endpoint: "https://registration.example"}
			if _, err := client.Issue(context.Background(), Request{Namespace: "payments", Project: "checkout", Destination: "telegram"}); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
