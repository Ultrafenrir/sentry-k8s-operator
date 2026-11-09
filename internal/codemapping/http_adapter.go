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

// Package codemapping owns Sentry repository path mappings and their optional imported
// CODEOWNERS children while keeping every external identity in protected state.
package codemapping

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type Integration struct {
	Identity string `json:"id"`
	Name     string `json:"name"`
	Provider struct {
		Key string `json:"key"`
	} `json:"provider"`
}

type responseIdentity string

func (id *responseIdentity) UnmarshalJSON(value []byte) error {
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		*id = responseIdentity(text)
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return err
	}
	*id = responseIdentity(number.String())
	return nil
}

func (value *Integration) UnmarshalJSON(payload []byte) error {
	var wire struct {
		Identity responseIdentity `json:"id"`
		Name     string           `json:"name"`
		Provider struct {
			Key string `json:"key"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return err
	}
	value.Identity = string(wire.Identity)
	value.Name = wire.Name
	value.Provider = wire.Provider
	return nil
}

type Repository struct {
	Identity      string `json:"id"`
	Name          string `json:"name"`
	IntegrationID string `json:"integrationId"`
}

func (value *Repository) UnmarshalJSON(payload []byte) error {
	var wire struct {
		Identity      responseIdentity `json:"id"`
		Name          string           `json:"name"`
		IntegrationID responseIdentity `json:"integrationId"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return err
	}
	value.Identity = string(wire.Identity)
	value.Name = wire.Name
	value.IntegrationID = string(wire.IntegrationID)
	return nil
}

type Remote struct {
	Identity       string `json:"id"`
	ProjectID      string `json:"projectId"`
	ProjectSlug    string `json:"projectSlug"`
	RepositoryID   string `json:"repoId"`
	RepositoryName string `json:"repoName"`
	IntegrationID  string `json:"integrationId"`
	Provider       struct {
		Key string `json:"key"`
	} `json:"provider"`
	StackRoot     string `json:"stackRoot"`
	SourceRoot    string `json:"sourceRoot"`
	DefaultBranch string `json:"defaultBranch"`
	HasCodeowners bool   `json:"hasCodeOwner"`
}

func (value *Remote) UnmarshalJSON(payload []byte) error {
	var wire struct {
		Identity       responseIdentity `json:"id"`
		ProjectID      responseIdentity `json:"projectId"`
		ProjectSlug    string           `json:"projectSlug"`
		RepositoryID   responseIdentity `json:"repoId"`
		RepositoryName string           `json:"repoName"`
		IntegrationID  responseIdentity `json:"integrationId"`
		Provider       struct {
			Key string `json:"key"`
		} `json:"provider"`
		StackRoot     string `json:"stackRoot"`
		SourceRoot    string `json:"sourceRoot"`
		DefaultBranch string `json:"defaultBranch"`
		HasCodeowners bool   `json:"hasCodeOwner"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return err
	}
	value.Identity = string(wire.Identity)
	value.ProjectID = string(wire.ProjectID)
	value.ProjectSlug = wire.ProjectSlug
	value.RepositoryID = string(wire.RepositoryID)
	value.RepositoryName = wire.RepositoryName
	value.IntegrationID = string(wire.IntegrationID)
	value.Provider = wire.Provider
	value.StackRoot = wire.StackRoot
	value.SourceRoot = wire.SourceRoot
	value.DefaultBranch = wire.DefaultBranch
	value.HasCodeowners = wire.HasCodeowners
	return nil
}

type MappingRequest struct {
	IntegrationID string `json:"integrationId"`
	RepositoryID  string `json:"repositoryId"`
	ProjectID     string `json:"projectId"`
	StackRoot     string `json:"stackRoot"`
	SourceRoot    string `json:"sourceRoot"`
	DefaultBranch string `json:"defaultBranch"`
}

type Codeowners struct {
	Identity      string `json:"id"`
	Raw           string `json:"raw"`
	CodeMappingID string `json:"codeMappingId"`
}

func (value *Codeowners) UnmarshalJSON(payload []byte) error {
	var wire struct {
		Identity      responseIdentity `json:"id"`
		Raw           string           `json:"raw"`
		CodeMappingID responseIdentity `json:"codeMappingId"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return err
	}
	value.Identity = string(wire.Identity)
	value.Raw = wire.Raw
	value.CodeMappingID = string(wire.CodeMappingID)
	return nil
}

type CodeownersSource struct {
	Raw string `json:"raw"`
}

type Adapter interface {
	ListIntegrations(context.Context, string, string) ([]Integration, error)
	ListRepositories(context.Context, string, string) ([]Repository, error)
	ListMappings(context.Context, string) ([]Remote, error)
	CreateMapping(context.Context, string, MappingRequest) (Remote, error)
	UpdateMapping(context.Context, string, string, MappingRequest) (Remote, error)
	DeleteMapping(context.Context, string, string) error
	FetchCodeowners(context.Context, string, string) (CodeownersSource, error)
	ListCodeowners(context.Context, string, string) ([]Codeowners, error)
	CreateCodeowners(context.Context, string, string, string, string) (Codeowners, error)
	UpdateCodeowners(context.Context, string, string, string, string, string) (Codeowners, error)
	DeleteCodeowners(context.Context, string, string, string) error
}

type HTTPAdapter struct{ Client *sentryapi.Client }

func (a *HTTPAdapter) ListIntegrations(ctx context.Context, organization, provider string) ([]Integration, error) {
	return sentryapi.CollectPages[Integration](ctx, a.Client, organizationPath(organization)+"integrations/?provider_key="+url.QueryEscape(provider), "list-source-code-integrations")
}

func (a *HTTPAdapter) ListRepositories(ctx context.Context, organization, integrationID string) ([]Repository, error) {
	return sentryapi.CollectPages[Repository](ctx, a.Client, organizationPath(organization)+"repos/?integration_id="+url.QueryEscape(integrationID), "list-source-code-repositories")
}

func (a *HTTPAdapter) ListMappings(ctx context.Context, organization string) ([]Remote, error) {
	return sentryapi.CollectPages[Remote](ctx, a.Client, mappingsPath(organization), "list-code-mappings")
}

func (a *HTTPAdapter) CreateMapping(ctx context.Context, organization string, request MappingRequest) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: mappingsPath(organization), Body: request, Operation: "create-code-mapping"}, &response)
	return response, err
}

func (a *HTTPAdapter) UpdateMapping(ctx context.Context, organization, identity string, request MappingRequest) (Remote, error) {
	var response Remote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: mappingPath(organization, identity), Body: request, Operation: "update-code-mapping"}, &response)
	return response, err
}

func (a *HTTPAdapter) DeleteMapping(ctx context.Context, organization, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: mappingPath(organization, identity), Operation: "delete-code-mapping"}, nil)
	return err
}

func (a *HTTPAdapter) FetchCodeowners(ctx context.Context, organization, mappingID string) (CodeownersSource, error) {
	var response CodeownersSource
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: mappingPath(organization, mappingID) + "codeowners/", RetrySafe: true, Operation: "fetch-codeowners"}, &response)
	return response, err
}

func (a *HTTPAdapter) ListCodeowners(ctx context.Context, organization, project string) ([]Codeowners, error) {
	return sentryapi.CollectPages[Codeowners](ctx, a.Client, codeownersPath(organization, project), "list-project-codeowners")
}

func (a *HTTPAdapter) CreateCodeowners(ctx context.Context, organization, project, mappingID, raw string) (Codeowners, error) {
	var response Codeowners
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: codeownersPath(organization, project), Body: map[string]string{"codeMappingId": mappingID, "raw": raw}, Operation: "create-project-codeowners"}, &response)
	return response, err
}

func (a *HTTPAdapter) UpdateCodeowners(ctx context.Context, organization, project, identity, mappingID, raw string) (Codeowners, error) {
	var response Codeowners
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: codeownersPath(organization, project) + url.PathEscape(identity) + "/", Body: map[string]string{"codeMappingId": mappingID, "raw": raw}, Operation: "update-project-codeowners"}, &response)
	return response, err
}

func (a *HTTPAdapter) DeleteCodeowners(ctx context.Context, organization, project, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: codeownersPath(organization, project) + url.PathEscape(identity) + "/", Operation: "delete-project-codeowners"}, nil)
	return err
}

func organizationPath(organization string) string {
	return "/api/0/organizations/" + url.PathEscape(organization) + "/"
}

func mappingsPath(organization string) string {
	return organizationPath(organization) + "code-mappings/"
}
func mappingPath(organization, identity string) string {
	return mappingsPath(organization) + url.PathEscape(identity) + "/"
}
func codeownersPath(organization, project string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/codeowners/"
}
