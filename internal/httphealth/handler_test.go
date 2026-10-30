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

package httphealth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrapServesOnlyExactGET(t *testing.T) {
	t.Parallel()
	delegated := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTeapot)
	})
	handler := Wrap(delegated)

	for _, test := range []struct {
		name   string
		method string
		target string
		want   int
	}{
		{name: "health", method: http.MethodGet, target: Path, want: http.StatusOK},
		{name: "head", method: http.MethodHead, target: Path, want: http.StatusTeapot},
		{name: "query", method: http.MethodGet, target: Path + "?full=true", want: http.StatusTeapot},
		{name: "other", method: http.MethodGet, target: "/healthz/", want: http.StatusTeapot},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(test.method, test.target, nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
			if test.want == http.StatusOK && recorder.Body.Len() != 0 {
				t.Fatalf("health response body length = %d, want 0", recorder.Body.Len())
			}
		})
	}
}
