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

// Package httphealth provides the deliberately small health endpoint shared by
// the standalone HTTPS services.
package httphealth

import "net/http"

const Path = "/healthz"

// Wrap serves an exact, bodyless GET health response and delegates every other
// request unchanged. Reaching this handler through the HTTPS listener proves
// that the listener and its dynamically loaded server certificate are usable.
func Wrap(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == Path && request.URL.RawQuery == "" {
			writer.Header().Set("Cache-Control", "no-store")
			writer.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
