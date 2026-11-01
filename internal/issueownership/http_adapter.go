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

package issueownership

import (
	"context"
	"net/http"
	"net/url"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

type HTTPAdapter struct {
	Client *sentryapi.Client
}

func (a *HTTPAdapter) Get(ctx context.Context, organization, projectSlug string) (State, error) {
	var response struct {
		Raw                *string `json:"raw"`
		Fallthrough        *bool   `json:"fallthrough"`
		AutoAssignment     *string `json:"autoAssignment"`
		CodeownersAutoSync *bool   `json:"codeownersAutoSync"`
	}
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodGet, Path: ownershipPath(organization, projectSlug),
		RetrySafe: true, Operation: "get-project-issue-ownership",
	}, &response)
	if err != nil {
		return State{}, err
	}
	empty := ""
	if response.Raw == nil {
		response.Raw = &empty
	}
	return State{
		Raw: response.Raw, Fallthrough: response.Fallthrough, AutoAssignment: response.AutoAssignment,
		CodeownersAutoSync: response.CodeownersAutoSync,
	}, nil
}

func (a *HTTPAdapter) Update(ctx context.Context, organization, projectSlug string, patch Patch) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut, Path: ownershipPath(organization, projectSlug), Body: patch,
		RetrySafe: false, Operation: "update-project-issue-ownership",
	}, nil)
	return err
}

func ownershipPath(organization, projectSlug string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(projectSlug) + "/ownership/"
}
