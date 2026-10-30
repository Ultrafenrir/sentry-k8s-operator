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

package settings

import (
	"context"
	"net/http"
	"net/url"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

type HTTPAdapter struct {
	Client *sentryapi.Client
}

func (a *HTTPAdapter) ListFilters(ctx context.Context, organization, projectSlug string) ([]Filter, error) {
	return sentryapi.CollectPages[Filter](ctx, a.Client, projectPath(organization, projectSlug)+"filters/", "list-project-filters")
}

func (a *HTTPAdapter) UpdateFilter(ctx context.Context, organization, projectSlug, id string, active bool) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut,
		Path:   projectPath(organization, projectSlug) + "filters/" + url.PathEscape(id) + "/",
		Body: struct {
			Active bool `json:"active"`
		}{Active: active},
		RetrySafe: false,
		Operation: "update-project-filter",
	}, nil)
	return err
}

func (a *HTTPAdapter) GetProjectState(ctx context.Context, organization, projectSlug string) (ProjectState, error) {
	var response struct {
		ScrubData        *bool     `json:"dataScrubber"`
		ScrubDefaults    *bool     `json:"dataScrubberDefaults"`
		ScrubIPAddresses *bool     `json:"scrubIPAddresses"`
		SensitiveFields  *[]string `json:"sensitiveFields"`
		SafeFields       *[]string `json:"safeFields"`
	}
	headers, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodGet,
		Path:      projectPath(organization, projectSlug),
		RetrySafe: true,
		Operation: "get-project-settings",
	}, &response)
	if err != nil {
		return ProjectState{}, err
	}
	return ProjectState{
		ScrubData:        response.ScrubData,
		ScrubDefaults:    response.ScrubDefaults,
		ScrubIPAddresses: response.ScrubIPAddresses,
		SensitiveFields:  response.SensitiveFields,
		SafeFields:       response.SafeFields,
		ETag:             headers.Get("ETag"),
	}, nil
}

func (a *HTTPAdapter) UpdateProjectState(ctx context.Context, organization, projectSlug string, patch ProjectPatch, etag string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodPut,
		Path:      projectPath(organization, projectSlug),
		Body:      patch,
		IfMatch:   etag,
		RetrySafe: false,
		Operation: "update-project-settings",
	}, nil)
	return err
}

func projectPath(organization, projectSlug string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(projectSlug) + "/"
}
