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

package sentryapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrorKind is a stable, body-free classification for Sentry API failures.
type ErrorKind string

const (
	ErrorInvalid          ErrorKind = "Invalid"
	ErrorUnauthorized     ErrorKind = "Unauthorized"
	ErrorPermissionDenied ErrorKind = "PermissionDenied"
	ErrorNotFound         ErrorKind = "NotFound"
	ErrorConflict         ErrorKind = "Conflict"
	ErrorRateLimited      ErrorKind = "RateLimited"
	ErrorServer           ErrorKind = "ServerError"
	ErrorTimeout          ErrorKind = "Timeout"
	ErrorTransport        ErrorKind = "TransportError"
	ErrorDecode           ErrorKind = "DecodeError"
)

// APIError intentionally excludes response bodies, tokens, DSNs, and complete URLs.
type APIError struct {
	Kind       ErrorKind
	StatusCode int
	RetryAfter time.Duration
	RequestID  string
	Operation  string
	underlying error
}

func (e *APIError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("Sentry API %s failed: %s (HTTP %d)", e.Operation, e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("Sentry API %s failed: %s", e.Operation, e.Kind)
}

func (e *APIError) Unwrap() error {
	return e.underlying
}

// IsKind reports whether an error has the requested safe classification.
func IsKind(err error, kind ErrorKind) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == kind
}

// AsAPIError safely exposes structured fields without parsing an error string.
func AsAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

func classifyStatus(status int) ErrorKind {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ErrorInvalid
	case http.StatusUnauthorized:
		return ErrorUnauthorized
	case http.StatusForbidden:
		return ErrorPermissionDenied
	case http.StatusNotFound:
		return ErrorNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return ErrorConflict
	case http.StatusTooManyRequests:
		return ErrorRateLimited
	default:
		if status >= 500 {
			return ErrorServer
		}
		return ErrorInvalid
	}
}
