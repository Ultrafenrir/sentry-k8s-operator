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

package project

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

// HTTPAdapter implements the source-confirmed public project endpoints.
type HTTPAdapter struct {
	Client *sentryapi.Client
}

type projectResponse struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	AbsoluteURL string `json:"absoluteUrl"`
}

func (a *HTTPAdapter) GetBySlug(ctx context.Context, organization, slug string) (RemoteProject, error) {
	var response projectResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodGet,
		Path:      "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		RetrySafe: true,
		Operation: "get-project",
	}, &response)
	if err != nil {
		return RemoteProject{}, err
	}
	return remoteFromResponse(response), nil
}

func (a *HTTPAdapter) Create(ctx context.Context, request CreateRequest) (RemoteProject, error) {
	managedName, err := managedProjectName(request)
	if err != nil {
		return RemoteProject{}, err
	}
	payload := struct {
		Name         string `json:"name"`
		Slug         string `json:"slug"`
		Platform     string `json:"platform,omitempty"`
		DefaultRules bool   `json:"default_rules"`
	}{
		Name:         managedName,
		Slug:         request.Slug,
		Platform:     request.Platform,
		DefaultRules: request.DefaultRules,
	}
	var response projectResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodPost,
		Path:      "/api/0/teams/" + url.PathEscape(request.Organization) + "/" + url.PathEscape(request.Team) + "/projects/",
		Body:      payload,
		RetrySafe: false,
		Operation: "create-project",
	}, &response)
	if err != nil {
		return RemoteProject{}, err
	}
	remote := remoteFromResponse(response)
	if remote.Identity == "" || remote.Slug != request.Slug || !matchesCreateEvidence(remote, request) {
		return RemoteProject{}, &sentryapi.APIError{Kind: sentryapi.ErrorDecode, StatusCode: http.StatusCreated, Operation: "create-project"}
	}
	return remote, nil
}

func managedProjectName(request CreateRequest) (string, error) {
	displayName := request.DisplayName
	if displayName == "" {
		displayName = request.Slug
	}
	if request.RegionalEvidence != nil {
		return FormatRegionalManagedName(displayName, *request.RegionalEvidence)
	}
	return FormatManagedNameFromSlug(displayName, request.Marker)
}

func matchesCreateEvidence(remote RemoteProject, request CreateRequest) bool {
	if request.RegionalEvidence != nil {
		return remote.RegionalEvidence != nil && *remote.RegionalEvidence == *request.RegionalEvidence
	}
	return remote.Marker != nil && request.Marker.Matches(*remote.Marker)
}

func (a *HTTPAdapter) Delete(ctx context.Context, organization, slug string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodDelete,
		Path:      "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		RetrySafe: false,
		Operation: "delete-project",
	}, nil)
	return err
}

// UpdateRegionalEvidence changes only the display name that carries compact ownership
// evidence. The engine performs the required read-after-write verification.
func (a *HTTPAdapter) UpdateRegionalEvidence(ctx context.Context, organization, slug string, evidence ownership.Evidence) error {
	name, err := FormatRegionalManagedNameFromSlug(slug, evidence)
	if err != nil {
		return err
	}
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut,
		Path:   "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		Body: struct {
			Name string `json:"name"`
		}{Name: name},
		Operation: "update-project-regional-evidence",
	}, nil)
	return err
}

func (a *HTTPAdapter) UpdateManagedName(ctx context.Context, organization, slug, name string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut,
		Path:   "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		Body: struct {
			Name string `json:"name"`
		}{Name: name},
		Operation: "update-project-managed-name",
	}, nil)
	return err
}

// ReleaseOwnership removes the internal marker by restoring the display name to the stable slug.
func (a *HTTPAdapter) ReleaseOwnership(ctx context.Context, organization, slug string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut,
		Path:   "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		Body: struct {
			Name string `json:"name"`
		}{Name: slug},
		Operation: "release-project-ownership",
	}, nil)
	return err
}

func remoteFromResponse(response projectResponse) RemoteProject {
	remote := RemoteProject{Identity: response.ID, Slug: response.Slug, Name: response.Name, URL: response.AbsoluteURL}
	if marker, found := ParseManagedName(response.Name); found {
		remote.Marker = &marker
	}
	if evidence, found := ParseRegionalManagedName(response.Name); found {
		remote.RegionalEvidence = &evidence
	}
	return remote
}
