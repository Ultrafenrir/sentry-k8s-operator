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

package sentryhook

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

var ErrServiceHook = errors.New("Sentry service hook is invalid")

const AlertEvent = "event.alert"

type RemoteServiceHook struct {
	Identity string
	URL      string
	Events   []string
	Active   bool
	Secret   []byte
}

type ServiceHookAdapter interface {
	List(context.Context, string, string) ([]RemoteServiceHook, error)
	Get(context.Context, string, string, string) (RemoteServiceHook, bool, error)
	Create(context.Context, string, string, string, bool) (RemoteServiceHook, error)
	Repair(context.Context, string, string, string, string) (RemoteServiceHook, error)
	Delete(context.Context, string, string, string) error
}

type HTTPServiceHookAdapter struct {
	Client *sentryapi.Client
}

type serviceHookResponse struct {
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Secret string   `json:"secret"`
	Events []string `json:"events"`
	Status string   `json:"status"`
}

type serviceHookRequest struct {
	URL      string   `json:"url"`
	Events   []string `json:"events"`
	Version  int      `json:"version"`
	IsActive bool     `json:"isActive"`
}

func (a *HTTPServiceHookAdapter) List(ctx context.Context, organization, project string) ([]RemoteServiceHook, error) {
	if a == nil || a.Client == nil {
		return nil, ErrServiceHook
	}
	items, err := sentryapi.CollectPages[serviceHookResponse](ctx, a.Client, serviceHookCollectionPath(organization, project), "list-project-service-hooks")
	if err != nil {
		return nil, err
	}
	result := make([]RemoteServiceHook, 0, len(items))
	for _, item := range items {
		remote, convertErr := remoteServiceHook(item)
		if convertErr != nil {
			return nil, convertErr
		}
		result = append(result, remote)
	}
	return result, nil
}

func (a *HTTPServiceHookAdapter) Get(ctx context.Context, organization, project, identity string) (RemoteServiceHook, bool, error) {
	if a == nil || a.Client == nil || invalidPathIdentity(identity) {
		return RemoteServiceHook{}, false, ErrServiceHook
	}
	var response serviceHookResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: serviceHookDetailsPath(organization, project, identity), RetrySafe: true, Operation: "get-project-service-hook"}, &response)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return RemoteServiceHook{}, false, nil
	}
	if err != nil {
		return RemoteServiceHook{}, false, err
	}
	remote, err := remoteServiceHook(response)
	return remote, err == nil, err
}

func (a *HTTPServiceHookAdapter) Create(ctx context.Context, organization, project, endpoint string, active bool) (RemoteServiceHook, error) {
	if a == nil || a.Client == nil || !validServiceHookEndpoint(endpoint) {
		return RemoteServiceHook{}, ErrServiceHook
	}
	var response serviceHookResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: serviceHookCollectionPath(organization, project), Body: desiredServiceHookRequest(endpoint, active), Operation: "create-project-service-hook"}, &response)
	if err != nil {
		return RemoteServiceHook{}, err
	}
	return remoteServiceHook(response)
}

func (a *HTTPServiceHookAdapter) Repair(ctx context.Context, organization, project, identity, endpoint string) (RemoteServiceHook, error) {
	if a == nil || a.Client == nil || invalidPathIdentity(identity) || !validServiceHookEndpoint(endpoint) {
		return RemoteServiceHook{}, ErrServiceHook
	}
	var response serviceHookResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: serviceHookDetailsPath(organization, project, identity), Body: desiredServiceHookRequest(endpoint, true), Operation: "repair-project-service-hook"}, &response)
	if err != nil {
		return RemoteServiceHook{}, err
	}
	return remoteServiceHook(response)
}

func (a *HTTPServiceHookAdapter) Delete(ctx context.Context, organization, project, identity string) error {
	if a == nil || a.Client == nil || invalidPathIdentity(identity) {
		return ErrServiceHook
	}
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: serviceHookDetailsPath(organization, project, identity), Operation: "delete-project-service-hook"}, nil)
	return err
}

func desiredServiceHookRequest(endpoint string, active bool) serviceHookRequest {
	return serviceHookRequest{URL: endpoint, Events: []string{AlertEvent}, Version: 0, IsActive: active}
}

func remoteServiceHook(response serviceHookResponse) (RemoteServiceHook, error) {
	events := append([]string(nil), response.Events...)
	slices.Sort(events)
	if invalidPathIdentity(response.ID) || !validServiceHookEndpoint(response.URL) || len(response.Secret) < 16 || len(response.Secret) > 512 || strings.ContainsAny(response.Secret, "\r\n\x00") || !slices.Equal(events, []string{AlertEvent}) || (response.Status != "active" && response.Status != "disabled") {
		return RemoteServiceHook{}, ErrServiceHook
	}
	return RemoteServiceHook{Identity: response.ID, URL: response.URL, Events: events, Active: response.Status == "active", Secret: []byte(response.Secret)}, nil
}

func serviceHookCollectionPath(organization, project string) string {
	return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(project) + "/hooks/"
}

func serviceHookDetailsPath(organization, project, identity string) string {
	return serviceHookCollectionPath(organization, project) + url.PathEscape(identity) + "/"
}

func invalidPathIdentity(value string) bool {
	return value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00")
}

func validServiceHookEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return false
	}
	sourceBindingKey := strings.TrimPrefix(parsed.Path, "/events/")
	return parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "/events/"+sourceBindingKey && canonicalHash(sourceBindingKey) && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

var _ ServiceHookAdapter = (*HTTPServiceHookAdapter)(nil)
