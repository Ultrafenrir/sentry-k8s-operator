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

// Package symbolsource owns portable custom symbol-source children without retaining
// credentials outside one reconciliation call.
package symbolsource

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type Layout struct {
	Type   string `json:"type"`
	Casing string `json:"casing"`
}

// Remote deliberately omits password, secret_key, and private_key. Sentry redacts those
// fields, and decoding them would create an unnecessary secret-bearing representation.
type Remote struct {
	Identity    string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Layout      Layout `json:"layout"`
	URL         string `json:"url,omitempty"`
	Username    string `json:"username,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Region      string `json:"region,omitempty"`
	AccessKey   string `json:"access_key,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	ClientEmail string `json:"client_email,omitempty"`
}

// Request is transient and may contain credentials. Callers must never persist, log, or
// expose this value.
type Request struct {
	Identity    string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Layout      Layout `json:"layout"`
	URL         string `json:"url,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Region      string `json:"region,omitempty"`
	AccessKey   string `json:"access_key,omitempty"`
	SecretKey   string `json:"secret_key,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	ClientEmail string `json:"client_email,omitempty"`
	PrivateKey  string `json:"private_key,omitempty"`
}

type Adapter interface {
	List(context.Context, string, string) ([]Remote, error)
	Create(context.Context, string, string, Request) (Remote, error)
	Update(context.Context, string, string, string, Request) (Remote, error)
	Delete(context.Context, string, string, string) error
}

type HTTPAdapter struct {
	Client *sentryapi.Client
}

func (a *HTTPAdapter) List(ctx context.Context, organization, project string) ([]Remote, error) {
	var response []Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodGet, Path: sourcesPath(organization, project), RetrySafe: true,
		Operation: "list-project-symbol-sources",
	}, &response)
	return response, err
}

func (a *HTTPAdapter) Create(ctx context.Context, organization, project string, request Request) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPost, Path: sourcesPath(organization, project), Body: request,
		RetrySafe: false, Operation: "create-project-symbol-source",
	}, &response)
	return response, err
}

func (a *HTTPAdapter) Update(ctx context.Context, organization, project, identity string, request Request) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut, Path: sourcePath(organization, project, identity), Body: request,
		RetrySafe: false, Operation: "update-project-symbol-source",
	}, &response)
	return response, err
}

func (a *HTTPAdapter) Delete(ctx context.Context, organization, project, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodDelete, Path: sourcePath(organization, project, identity),
		RetrySafe: false, Operation: "delete-project-symbol-source",
	}, nil)
	return err
}

func sourcesPath(organization, project string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/symbol-sources/"
}

func sourcePath(organization, project, identity string) string {
	return sourcesPath(organization, project) + "?id=" + url.QueryEscape(identity)
}
