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

package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

const (
	LegacyIssueAdapter  = "legacy-issue-24.7.1"
	LegacyMetricAdapter = "legacy-metric-24.7.1"
)

var (
	ErrLegacyInvalidState           = errors.New("legacy alert state is invalid")
	ErrLegacyDestinationUnsupported = errors.New("legacy alert destination is unsupported")
	ErrLegacyToggleUnsupported      = errors.New("legacy alert disable is unsupported")
)

// LegacyHTTPAdapter implements the source- and live-confirmed Sentry 24.7.1 issue and
// metric alert CRUD contracts. Sentry 24.7.1 has no non-destructive metric toggle and no
// issue-rule disable endpoint; callers therefore must never advertise SupportsToggle.
type LegacyHTTPAdapter struct {
	Client       *sentryapi.Client
	Destinations map[string]binding.DestinationBinding
}

type legacyID string

func (id *legacyID) UnmarshalJSON(value []byte) error {
	var text string
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		var number json.Number
		if err := decoder.Decode(&number); err != nil {
			return err
		}
		text = number.String()
	}
	if !numericIdentityPattern.MatchString(text) {
		return ErrLegacyInvalidState
	}
	*id = legacyID(text)
	return nil
}

type legacyIssueResponse struct {
	ID     legacyID `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
}

type legacyIssueAction struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace,omitempty"`
	Channel   string `json:"channel,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Tags      string `json:"tags,omitempty"`
	Notes     string `json:"notes,omitempty"`
	Account   string `json:"account,omitempty"`
	Service   string `json:"service,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

type legacyIssuePayload struct {
	Name        string              `json:"name"`
	ActionMatch string              `json:"actionMatch"`
	FilterMatch string              `json:"filterMatch"`
	Conditions  []map[string]string `json:"conditions"`
	Filters     []any               `json:"filters"`
	Actions     []legacyIssueAction `json:"actions"`
	Frequency   int                 `json:"frequency"`
}

type legacyMetricResponse struct {
	ID       legacyID              `json:"id"`
	Name     string                `json:"name"`
	Status   int                   `json:"status"`
	Triggers []legacyMetricTrigger `json:"triggers"`
}

type legacyMetricAction struct {
	Type             string `json:"type"`
	TargetType       string `json:"targetType"`
	TargetIdentifier any    `json:"targetIdentifier"`
	IntegrationID    int64  `json:"integrationId"`
	InputChannelID   string `json:"inputChannelId,omitempty"`
}

type legacyMetricTrigger struct {
	Label          string               `json:"label"`
	AlertThreshold float64              `json:"alertThreshold"`
	Actions        []legacyMetricAction `json:"actions"`
}

type legacyMetricPayload struct {
	Name             string                `json:"name"`
	QueryType        int                   `json:"queryType"`
	Dataset          string                `json:"dataset"`
	Aggregate        string                `json:"aggregate"`
	Query            string                `json:"query"`
	TimeWindow       int                   `json:"timeWindow"`
	ResolveThreshold int64                 `json:"resolveThreshold"`
	ThresholdType    int                   `json:"thresholdType"`
	ThresholdPeriod  int                   `json:"thresholdPeriod"`
	Projects         []string              `json:"projects"`
	EventTypes       []string              `json:"eventTypes"`
	Triggers         []legacyMetricTrigger `json:"triggers"`
}

func (a *LegacyHTTPAdapter) Ensure(ctx context.Context, adapter, organization, projectSlug, key string, desired Desired, marker binding.Marker) (Observed, error) {
	if err := a.validate(adapter, projectSlug, key, desired); err != nil {
		return Observed{}, err
	}
	name, err := formatModernName(projectSlug, key, desiredHash(normalize(desired)), marker)
	if err != nil {
		return Observed{}, ErrLegacyInvalidState
	}
	items, err := a.list(ctx, adapter, organization, projectSlug)
	if err != nil {
		return Observed{}, err
	}
	matches := matchingLegacy(items, name)
	if len(matches) > 1 {
		return Observed{}, ErrLegacyInvalidState
	}
	if len(matches) == 1 {
		return a.ObserveExisting(adapter, matches[0], projectSlug)
	}

	identity, err := a.create(ctx, adapter, organization, projectSlug, name, desired)
	if err != nil {
		return Observed{}, err
	}
	return a.observeRequired(ctx, adapter, organization, projectSlug, identity)
}

func (a *LegacyHTTPAdapter) Observe(ctx context.Context, adapter, organization, projectSlug, identity string) (Observed, bool, error) {
	if a.Client == nil || !legacyAdapter(adapter) || !numericIdentityPattern.MatchString(identity) {
		return Observed{}, false, ErrLegacyInvalidState
	}
	response, found, err := a.getExisting(ctx, adapter, organization, projectSlug, identity)
	if !found || err != nil {
		return Observed{}, found, err
	}
	if issue, ok := response.(*legacyIssueResponse); ok && strings.EqualFold(issue.Status, "disabled") {
		return Observed{}, false, nil
	}
	if metric, ok := response.(*legacyMetricResponse); ok && metric.Status == 5 {
		return Observed{}, false, nil
	}
	observed, err := a.ObserveExisting(adapter, response, projectSlug)
	return observed, err == nil, err
}

func (a *LegacyHTTPAdapter) Update(ctx context.Context, adapter, organization, projectSlug, identity string, desired Desired) (Observed, error) {
	if err := a.validate(adapter, projectSlug, "rule", desired); err != nil || !numericIdentityPattern.MatchString(identity) {
		return Observed{}, errors.Join(ErrLegacyInvalidState, err)
	}
	currentValue, found, err := a.getExisting(ctx, adapter, organization, projectSlug, identity)
	if err != nil || !found {
		return Observed{}, errors.Join(ErrLegacyInvalidState, err)
	}
	current, err := a.ObserveExisting(adapter, currentValue, projectSlug)
	if err != nil {
		return Observed{}, err
	}
	var currentName string
	switch response := currentValue.(type) {
	case *legacyIssueResponse:
		currentName = response.Name
	case *legacyMetricResponse:
		currentName = response.Name
	}
	parsed, ok := parseModernName(currentName)
	if !ok {
		return Observed{}, ErrLegacyInvalidState
	}
	name, err := formatModernName(projectSlug, parsed.Key, desiredHash(normalize(desired)), current.Marker)
	if err != nil {
		return Observed{}, ErrLegacyInvalidState
	}
	body, err := a.payload(adapter, projectSlug, name, desired)
	if err != nil {
		return Observed{}, err
	}
	var response any
	if adapter == LegacyIssueAdapter {
		response = &legacyIssueResponse{}
	} else {
		response = &legacyMetricResponse{}
	}
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: a.detailPath(adapter, organization, projectSlug, identity), Body: body, Operation: "update-legacy-alert"}, response)
	if err != nil {
		return Observed{}, err
	}
	return a.observeRequired(ctx, adapter, organization, projectSlug, identity)
}

func (a *LegacyHTTPAdapter) SetEnabled(ctx context.Context, adapter, organization, projectSlug, identity string, enabled bool) (Observed, error) {
	if !enabled {
		return Observed{}, ErrLegacyToggleUnsupported
	}
	if adapter != LegacyIssueAdapter || !numericIdentityPattern.MatchString(identity) {
		return Observed{}, ErrLegacyToggleUnsupported
	}
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: a.detailPath(adapter, organization, projectSlug, identity) + "enable/", Operation: "enable-legacy-issue-alert"}, nil)
	if err != nil {
		return Observed{}, err
	}
	return a.observeRequired(ctx, adapter, organization, projectSlug, identity)
}

func (a *LegacyHTTPAdapter) Delete(ctx context.Context, adapter, organization, projectSlug, identity string, marker binding.Marker) error {
	observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, identity)
	if err != nil || !found {
		return err
	}
	if !observed.Marker.Matches(marker) {
		return ErrLegacyInvalidState
	}
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: a.detailPath(adapter, organization, projectSlug, identity), Operation: "delete-legacy-alert"}, nil)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return nil
	}
	return err
}

func (a *LegacyHTTPAdapter) ObserveExisting(adapter string, value any, projectSlug string) (Observed, error) {
	var identity, name string
	switch response := value.(type) {
	case *legacyIssueResponse:
		identity, name = string(response.ID), response.Name
	case legacyIssueResponse:
		identity, name = string(response.ID), response.Name
	case *legacyMetricResponse:
		identity, name = string(response.ID), response.Name
	case legacyMetricResponse:
		identity, name = string(response.ID), response.Name
	default:
		return Observed{}, ErrLegacyInvalidState
	}
	parsed, ok := parseModernName(name)
	if !ok || parsed.Project != projectSlug || !numericIdentityPattern.MatchString(identity) {
		return Observed{}, ErrLegacyInvalidState
	}
	return Observed{Identity: identity, Adapter: adapter, Marker: parsed.Marker, DesiredHash: parsed.Hash, Enabled: true}, nil
}

func (a *LegacyHTTPAdapter) observeRequired(ctx context.Context, adapter, organization, projectSlug, identity string) (Observed, error) {
	observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, identity)
	if err != nil || !found {
		return Observed{}, errors.Join(ErrLegacyInvalidState, err)
	}
	return observed, nil
}

func (a *LegacyHTTPAdapter) getExisting(ctx context.Context, adapter, organization, projectSlug, identity string) (any, bool, error) {
	var response any
	if adapter == LegacyIssueAdapter {
		response = &legacyIssueResponse{}
	} else {
		response = &legacyMetricResponse{}
	}
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: a.detailPath(adapter, organization, projectSlug, identity), RetrySafe: true, Operation: "get-legacy-alert"}, response)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return response, true, nil
}

func (a *LegacyHTTPAdapter) validate(adapter, projectSlug, key string, desired Desired) error {
	if a.Client == nil || !legacyAdapter(adapter) || !dnsLabel(projectSlug) || !dnsLabel(key) || !desired.Enabled {
		return ErrLegacyInvalidState
	}
	if adapter == LegacyIssueAdapter && desired.Trigger != TriggerFirstSeen && desired.Trigger != TriggerRegression {
		return ErrLegacyInvalidState
	}
	if adapter == LegacyMetricAdapter {
		if _, ok := metricAggregate(desired.Trigger); !ok || desired.WindowMinutes < 1 || desired.WindowMinutes > 1440 || desired.Threshold <= 0 {
			return ErrLegacyInvalidState
		}
	}
	_, err := a.destinationActions(adapter, desired.Notify)
	return err
}

func (a *LegacyHTTPAdapter) payload(adapter, projectSlug, name string, desired Desired) (any, error) {
	actions, err := a.destinationActions(adapter, desired.Notify)
	if err != nil {
		return nil, err
	}
	if adapter == LegacyIssueAdapter {
		condition := "sentry.rules.conditions.first_seen_event.FirstSeenEventCondition"
		if desired.Trigger == TriggerRegression {
			condition = "sentry.rules.conditions.regression_event.RegressionEventCondition"
		}
		return legacyIssuePayload{Name: name, ActionMatch: "all", FilterMatch: "all", Conditions: []map[string]string{{"id": condition}}, Filters: []any{}, Actions: actions.([]legacyIssueAction), Frequency: 30}, nil
	}
	aggregate, ok := metricAggregate(desired.Trigger)
	if !ok {
		return nil, ErrLegacyInvalidState
	}
	return legacyMetricPayload{
		Name: name, QueryType: 0, Dataset: "events", Aggregate: aggregate, Query: "",
		TimeWindow: desired.WindowMinutes, ResolveThreshold: 0, ThresholdType: 0,
		ThresholdPeriod: 1, Projects: []string{projectSlug}, EventTypes: []string{"error"},
		Triggers: []legacyMetricTrigger{{Label: "critical", AlertThreshold: float64(desired.Threshold), Actions: actions.([]legacyMetricAction)}},
	}, nil
}

func (a *LegacyHTTPAdapter) destinationActions(adapter string, names []string) (any, error) {
	issue := make([]legacyIssueAction, 0, len(names))
	metric := make([]legacyMetricAction, 0, len(names))
	for _, name := range names {
		destination, ok := a.Destinations[name]
		if !ok || destination.State != "Active" {
			return nil, ErrLegacyDestinationUnsupported
		}
		if !numericIdentityPattern.MatchString(destination.IntegrationIdentity) || destination.ResourceIdentity == "" {
			return nil, ErrLegacyDestinationUnsupported
		}
		integrationID, err := strconv.ParseInt(destination.IntegrationIdentity, 10, 64)
		if err != nil || integrationID <= 0 {
			return nil, ErrLegacyDestinationUnsupported
		}
		switch destination.Kind {
		case "slack":
			if destination.ResourceName == "" {
				return nil, ErrLegacyDestinationUnsupported
			}
			issue = append(issue, legacyIssueAction{ID: "sentry.integrations.slack.notify_action.SlackNotifyServiceAction", Workspace: destination.IntegrationIdentity, Channel: destination.ResourceName, ChannelID: destination.ResourceIdentity})
			metric = append(metric, legacyMetricAction{Type: "slack", TargetType: "specific", TargetIdentifier: destination.ResourceName, IntegrationID: integrationID, InputChannelID: destination.ResourceIdentity})
		case "pagerduty":
			resourceID, parseErr := strconv.ParseInt(destination.ResourceIdentity, 10, 64)
			if parseErr != nil || resourceID <= 0 {
				return nil, ErrLegacyDestinationUnsupported
			}
			issue = append(issue, legacyIssueAction{ID: "sentry.integrations.pagerduty.notify_action.PagerDutyNotifyServiceAction", Account: destination.IntegrationIdentity, Service: destination.ResourceIdentity, Severity: "default"})
			metric = append(metric, legacyMetricAction{Type: "pagerduty", TargetType: "specific", TargetIdentifier: resourceID, IntegrationID: integrationID})
		default:
			return nil, ErrLegacyDestinationUnsupported
		}
	}
	if adapter == LegacyIssueAdapter {
		return issue, nil
	}
	return metric, nil
}

func (a *LegacyHTTPAdapter) create(ctx context.Context, adapter, organization, projectSlug, name string, desired Desired) (string, error) {
	body, err := a.payload(adapter, projectSlug, name, desired)
	if err != nil {
		return "", err
	}
	var response any
	path := a.collectionPath(adapter, organization, projectSlug)
	if adapter == LegacyIssueAdapter {
		response = &legacyIssueResponse{}
	} else {
		response = &legacyMetricResponse{}
	}
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: path, Body: body, Operation: "create-legacy-alert"}, response)
	if err != nil {
		return "", err
	}
	observed, err := a.ObserveExisting(adapter, response, projectSlug)
	if err != nil {
		return "", err
	}
	return observed.Identity, nil
}

func (a *LegacyHTTPAdapter) list(ctx context.Context, adapter, organization, projectSlug string) ([]any, error) {
	if a.Client == nil || !legacyAdapter(adapter) {
		return nil, ErrLegacyInvalidState
	}
	if adapter == LegacyIssueAdapter {
		response, err := sentryapi.CollectPages[legacyIssueResponse](ctx, a.Client, a.collectionPath(adapter, organization, projectSlug), "list-legacy-issue-alerts")
		items := make([]any, len(response))
		for index := range response {
			items[index] = response[index]
		}
		return items, err
	}
	response, err := sentryapi.CollectPages[legacyMetricResponse](ctx, a.Client, a.collectionPath(adapter, organization, projectSlug), "list-legacy-metric-alerts")
	items := make([]any, len(response))
	for index := range response {
		items[index] = response[index]
	}
	return items, err
}

func (a *LegacyHTTPAdapter) collectionPath(adapter, organization, projectSlug string) string {
	if adapter == LegacyIssueAdapter {
		return "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(projectSlug) + "/rules/"
	}
	return organizationPath(organization) + "alert-rules/"
}

func (a *LegacyHTTPAdapter) detailPath(adapter, organization, projectSlug, identity string) string {
	return a.collectionPath(adapter, organization, projectSlug) + url.PathEscape(identity) + "/"
}

func matchingLegacy(items []any, name string) []any {
	result := make([]any, 0, 1)
	for _, item := range items {
		var candidate string
		switch value := item.(type) {
		case legacyIssueResponse:
			candidate = value.Name
		case legacyMetricResponse:
			candidate = value.Name
		}
		if candidate == name {
			result = append(result, item)
		}
	}
	return result
}

func legacyAdapter(adapter string) bool {
	return adapter == LegacyIssueAdapter || adapter == LegacyMetricAdapter
}
