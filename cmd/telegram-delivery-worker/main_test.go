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

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/telegramdelivery"
)

func TestMetricsHandlerServesOnlyExactBodylessGetRoute(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	if _, err := telegramdelivery.NewMetrics(registry); err != nil {
		t.Fatal(err)
	}
	handler := metricsHandler(promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	request := httptest.NewRequest(http.MethodGet, "http://worker/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sentry_operator_telegram_delivery_queue_records") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metrics response = %d %q", response.Code, response.Body.String())
	}

	for _, target := range []string{"http://worker/metrics?full=true", "http://worker/private-payload-SHOULD-NOT-LEAK"} {
		request = httptest.NewRequest(http.MethodGet, target, nil)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || response.Body.Len() != 0 {
			t.Fatalf("non-exact response = %d %q", response.Code, response.Body.String())
		}
	}
}
