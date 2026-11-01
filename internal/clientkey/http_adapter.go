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

// Package clientkey owns server-generated Sentry project client keys without retaining
// secret key material or private DSNs returned by Sentry.
package clientkey

import (
	"context"
	"net/http"
	"net/url"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

type RateLimit struct {
	Count  int64 `json:"count"`
	Window int32 `json:"window"`
}

// Remote deliberately allowlists only lifecycle fields and the public DSN. Sentry's
// response also contains secret key material and private endpoints; encoding/json discards
// them because this type has no destination fields for them.
type Remote struct {
	Identity    string     `json:"id"`
	Name        string     `json:"name"`
	IsActive    bool       `json:"isActive"`
	RateLimit   *RateLimit `json:"rateLimit"`
	DateCreated string     `json:"dateCreated"`
	DSN         struct {
		Public string `json:"public"`
	} `json:"dsn"`
}

type CreateRequest struct {
	Name      string     `json:"name"`
	RateLimit *RateLimit `json:"rateLimit,omitempty"`
}

type UpdateRequest struct {
	Name      string     `json:"name"`
	IsActive  bool       `json:"isActive"`
	RateLimit *RateLimit `json:"rateLimit"`
}

type Adapter interface {
	List(context.Context, string, string) ([]Remote, error)
	Create(context.Context, string, string, CreateRequest) (Remote, error)
	Get(context.Context, string, string, string) (Remote, bool, error)
	Update(context.Context, string, string, string, UpdateRequest) (Remote, error)
	Delete(context.Context, string, string, string) error
}

type HTTPAdapter struct {
	Client *sentryapi.Client
}

func (a *HTTPAdapter) List(ctx context.Context, organization, project string) ([]Remote, error) {
	return sentryapi.CollectPages[Remote](ctx, a.Client, keysPath(organization, project), "list-project-client-keys")
}

func (a *HTTPAdapter) Create(ctx context.Context, organization, project string, request CreateRequest) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPost, Path: keysPath(organization, project), Body: request,
		RetrySafe: false, Operation: "create-project-client-key",
	}, &response)
	return response, err
}

func (a *HTTPAdapter) Get(ctx context.Context, organization, project, identity string) (Remote, bool, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodGet, Path: keyPath(organization, project, identity),
		RetrySafe: true, Operation: "get-project-client-key",
	}, &response)
	if err == nil {
		return response, true, nil
	}
	var apiErr *sentryapi.APIError
	if sentryapi.AsAPIError(err, &apiErr) && apiErr.Kind == sentryapi.ErrorNotFound {
		return Remote{}, false, nil
	}
	return Remote{}, false, err
}

func (a *HTTPAdapter) Update(ctx context.Context, organization, project, identity string, request UpdateRequest) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut, Path: keyPath(organization, project, identity), Body: request,
		RetrySafe: false, Operation: "update-project-client-key",
	}, &response)
	return response, err
}

func (a *HTTPAdapter) Delete(ctx context.Context, organization, project, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodDelete, Path: keyPath(organization, project, identity),
		RetrySafe: false, Operation: "delete-project-client-key",
	}, nil)
	return err
}

func keysPath(organization, project string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/keys/"
}

func keyPath(organization, project, identity string) string {
	return keysPath(organization, project) + url.PathEscape(identity) + "/"
}
