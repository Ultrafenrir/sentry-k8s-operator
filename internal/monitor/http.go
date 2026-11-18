/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package monitor implements the bounded Cron and Uptime remote contracts.
package monitor

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type Actor struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type CronConfig struct {
	ScheduleType      string `json:"schedule_type"`
	Schedule          any    `json:"schedule"`
	CheckInMargin     *int32 `json:"checkin_margin"`
	MaxRuntime        *int32 `json:"max_runtime"`
	Timezone          string `json:"timezone,omitempty"`
	FailureThreshold  *int32 `json:"failure_issue_threshold"`
	RecoveryThreshold *int32 `json:"recovery_threshold"`
}

type CronRequest struct {
	Project string     `json:"project"`
	Type    string     `json:"type"`
	Name    string     `json:"name"`
	Slug    string     `json:"slug"`
	Status  string     `json:"status"`
	Owner   *string    `json:"owner,omitempty"`
	Config  CronConfig `json:"config"`
}

type CronRemote struct {
	Identity string     `json:"id"`
	Name     string     `json:"name"`
	Slug     string     `json:"slug"`
	Status   string     `json:"status"`
	Config   CronConfig `json:"config"`
	Project  struct {
		Slug string `json:"slug"`
	} `json:"project"`
	Owner *Actor `json:"owner"`
}

type UptimeRequest struct {
	Name                   string      `json:"name"`
	Status                 string      `json:"status"`
	Owner                  *string     `json:"owner,omitempty"`
	Environment            *string     `json:"environment,omitempty"`
	URL                    string      `json:"url"`
	IntervalSeconds        int32       `json:"intervalSeconds"`
	TimeoutMilliseconds    int32       `json:"timeoutMs"`
	Method                 string      `json:"method"`
	Headers                [][2]string `json:"headers"`
	Body                   *string     `json:"body"`
	TraceSampling          bool        `json:"traceSampling"`
	ResponseCaptureEnabled bool        `json:"responseCaptureEnabled"`
	RecoveryThreshold      int32       `json:"recoveryThreshold"`
	DowntimeThreshold      int32       `json:"downtimeThreshold"`
	Assertion              any         `json:"assertion"`
}

type UptimeRemote struct {
	Identity               string      `json:"id"`
	ProjectSlug            string      `json:"projectSlug"`
	Name                   string      `json:"name"`
	Status                 string      `json:"status"`
	Owner                  *Actor      `json:"owner"`
	Environment            *string     `json:"environment"`
	URL                    string      `json:"url"`
	IntervalSeconds        int32       `json:"intervalSeconds"`
	TimeoutMilliseconds    int32       `json:"timeoutMs"`
	Method                 string      `json:"method"`
	Headers                [][2]string `json:"headers"`
	Body                   *string     `json:"body"`
	TraceSampling          bool        `json:"traceSampling"`
	ResponseCaptureEnabled bool        `json:"responseCaptureEnabled"`
	RecoveryThreshold      int32       `json:"recoveryThreshold"`
	DowntimeThreshold      int32       `json:"downtimeThreshold"`
	Assertion              any         `json:"assertion"`
}

type HTTPAdapter struct{ Client *sentryapi.Client }

func (a *HTTPAdapter) ResolveTeam(ctx context.Context, organization, slug string) (string, bool, error) {
	teams, err := sentryapi.CollectPages[Actor](ctx, a.Client, "/api/0/organizations/"+url.PathEscape(organization)+"/teams/", "list-monitor-teams")
	if err != nil {
		return "", false, err
	}
	for _, team := range teams {
		if team.Slug == slug {
			return team.ID, true, nil
		}
	}
	return "", false, nil
}

func (a *HTTPAdapter) ListCron(ctx context.Context, organization, projectIdentity string) ([]CronRemote, error) {
	path := "/api/0/organizations/" + url.PathEscape(organization) + "/monitors/?project=" + url.QueryEscape(projectIdentity)
	return sentryapi.CollectPages[CronRemote](ctx, a.Client, path, "list-cron-monitors")
}

func (a *HTTPAdapter) CreateCron(ctx context.Context, organization string, request CronRequest) (CronRemote, error) {
	var response CronRemote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: "/api/0/organizations/" + url.PathEscape(organization) + "/monitors/", Body: request, Operation: "create-cron-monitor"}, &response)
	return response, err
}

func (a *HTTPAdapter) GetCron(ctx context.Context, organization, project, identity string) (CronRemote, bool, error) {
	var response CronRemote
	path := cronPath(organization, project, identity)
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: path, RetrySafe: true, Operation: "get-cron-monitor"}, &response)
	if notFound(err) {
		return CronRemote{}, false, nil
	}
	return response, err == nil, err
}

func (a *HTTPAdapter) UpdateCron(ctx context.Context, organization, project, identity string, request CronRequest) (CronRemote, error) {
	var response CronRemote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: cronPath(organization, project, identity), Body: request, Operation: "update-cron-monitor"}, &response)
	return response, err
}

func (a *HTTPAdapter) DeleteCron(ctx context.Context, organization, project, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: cronPath(organization, project, identity), Operation: "delete-cron-monitor"}, nil)
	return err
}

func (a *HTTPAdapter) ListUptime(ctx context.Context, organization, project string) ([]UptimeRemote, error) {
	path := "/api/0/organizations/" + url.PathEscape(organization) + "/uptime/?project=" + url.QueryEscape(project)
	return sentryapi.CollectPages[UptimeRemote](ctx, a.Client, path, "list-uptime-monitors")
}

func (a *HTTPAdapter) PreflightUptime(ctx context.Context, organization string, request UptimeRequest) error {
	body := map[string]any{"url": request.URL, "timeoutMs": request.TimeoutMilliseconds, "method": request.Method, "headers": request.Headers, "body": request.Body, "assertion": request.Assertion}
	var response map[string]any
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: "/api/0/organizations/" + url.PathEscape(organization) + "/uptime-preview-check/", Body: body, Operation: "preflight-uptime-monitor"}, &response)
	return err
}

func (a *HTTPAdapter) CreateUptime(ctx context.Context, organization, project string, request UptimeRequest) (UptimeRemote, error) {
	var response UptimeRemote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: uptimeBasePath(organization, project), Body: request, Operation: "create-uptime-monitor"}, &response)
	return response, err
}

func (a *HTTPAdapter) GetUptime(ctx context.Context, organization, project, identity string) (UptimeRemote, bool, error) {
	var response UptimeRemote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: uptimePath(organization, project, identity), RetrySafe: true, Operation: "get-uptime-monitor"}, &response)
	if notFound(err) {
		return UptimeRemote{}, false, nil
	}
	return response, err == nil, err
}

func (a *HTTPAdapter) UpdateUptime(ctx context.Context, organization, project, identity string, request UptimeRequest) (UptimeRemote, error) {
	var response UptimeRemote
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: uptimePath(organization, project, identity), Body: request, Operation: "update-uptime-monitor"}, &response)
	return response, err
}

func (a *HTTPAdapter) DeleteUptime(ctx context.Context, organization, project, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: uptimePath(organization, project, identity), Operation: "delete-uptime-monitor"}, nil)
	return err
}

func cronPath(organization, project, identity string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/monitors/" + url.PathEscape(identity) + "/"
}

func uptimeBasePath(organization, project string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/uptime/"
}

func uptimePath(organization, project, identity string) string {
	return uptimeBasePath(organization, project) + url.PathEscape(identity) + "/"
}

func notFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *sentryapi.APIError
	return sentryapi.AsAPIError(err, &apiErr) && apiErr.Kind == sentryapi.ErrorNotFound
}
