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

package projectaccess

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type HTTPAdapter struct {
	Client *sentryapi.Client
}

type teamResponse struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

func (a *HTTPAdapter) ListTeams(ctx context.Context, organization, projectSlug string) ([]Team, error) {
	path := projectTeamsPath(organization, projectSlug)
	responses, err := sentryapi.CollectPages[teamResponse](ctx, a.Client, path, "list-project-teams")
	if err != nil {
		return nil, err
	}
	result := make([]Team, 0, len(responses))
	for _, response := range responses {
		result = append(result, Team{Identity: response.ID, Slug: response.Slug})
	}
	return result, nil
}

func (a *HTTPAdapter) GrantTeam(ctx context.Context, organization, projectSlug, teamSlug string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPost, Path: projectTeamPath(organization, projectSlug, teamSlug),
		RetrySafe: false, Operation: "grant-project-team",
	}, nil)
	return err
}

func (a *HTTPAdapter) RevokeTeam(ctx context.Context, organization, projectSlug, teamIdentity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodDelete, Path: projectTeamPath(organization, projectSlug, teamIdentity),
		RetrySafe: false, Operation: "revoke-project-team",
	}, nil)
	return err
}

func projectTeamsPath(organization, projectSlug string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(projectSlug) + "/teams/"
}

func projectTeamPath(organization, projectSlug, team string) string {
	return projectTeamsPath(organization, projectSlug) + url.PathEscape(team) + "/"
}
