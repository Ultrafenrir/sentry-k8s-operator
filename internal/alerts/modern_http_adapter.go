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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

const ModernMetricIssueAdapter = "modern-metric-issue-v1"

var (
	ErrModernInvalidState           = errors.New("modern metric alert state is invalid")
	ErrModernDestinationUnsupported = errors.New("modern metric alert destination is unsupported")
	managedAlertNamePattern         = regexp.MustCompile(`^sko:([a-z0-9](?:[a-z0-9-]*[a-z0-9])?):([a-z](?:[a-z0-9-]*[a-z0-9])?):([0-9a-f]{64}) \[managed;o=([a-z](?:[a-z0-9.-]*[a-z0-9])?);m=([a-z](?:[a-z0-9.-]*[a-z0-9])?)\]$`)
	numericIdentityPattern          = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// ModernHTTPAdapter implements the source- and live-confirmed Sentry 26 metric_issue
// workflow/detector lifecycle. One protected identity contains both remote children.
type ModernHTTPAdapter struct {
	Client       *sentryapi.Client
	Destinations map[string]binding.DestinationBinding
}

type modernIdentity struct {
	Detector string `json:"detector"`
	Workflow string `json:"workflow"`
}

type modernName struct {
	Project string
	Key     string
	Hash    string
	Marker  binding.Marker
}

type modernWorkflowResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Enabled     bool     `json:"enabled"`
	DetectorIDs []string `json:"detectorIds"`
}

type modernDetectorResponse struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"projectId"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Enabled     bool     `json:"enabled"`
	WorkflowIDs []string `json:"workflowIds"`
}

type modernCondition struct {
	Type            string `json:"type"`
	Comparison      int64  `json:"comparison"`
	ConditionResult int    `json:"conditionResult"`
}

type modernConditionGroup struct {
	LogicType  string            `json:"logicType"`
	Conditions []modernCondition `json:"conditions"`
}

type modernAction struct {
	Type          string         `json:"type"`
	IntegrationID int64          `json:"integrationId"`
	Data          map[string]any `json:"data"`
	Config        map[string]any `json:"config"`
}

type modernActionFilter struct {
	LogicType  string            `json:"logicType"`
	Conditions []modernCondition `json:"conditions"`
	Actions    []modernAction    `json:"actions"`
}

type modernWorkflowPayload struct {
	Name          string               `json:"name"`
	Enabled       bool                 `json:"enabled"`
	Config        map[string]any       `json:"config"`
	Triggers      modernConditionGroup `json:"triggers"`
	ActionFilters []modernActionFilter `json:"actionFilters"`
}

type modernDataSource struct {
	QueryType   int      `json:"queryType"`
	Dataset     string   `json:"dataset"`
	Query       string   `json:"query"`
	Aggregate   string   `json:"aggregate"`
	TimeWindow  int      `json:"timeWindow"`
	Environment *string  `json:"environment"`
	EventTypes  []string `json:"eventTypes"`
}

type modernDetectorPayload struct {
	Name           string               `json:"name"`
	Type           string               `json:"type"`
	Enabled        bool                 `json:"enabled"`
	DataSources    []modernDataSource   `json:"dataSources"`
	ConditionGroup modernConditionGroup `json:"conditionGroup"`
	Config         map[string]any       `json:"config"`
	WorkflowIDs    []string             `json:"workflowIds"`
}

func (a *ModernHTTPAdapter) Ensure(ctx context.Context, adapter, organization, projectSlug, key string, desired Desired, marker binding.Marker) (Observed, error) {
	if err := a.validate(adapter, projectSlug, key, desired); err != nil {
		return Observed{}, err
	}
	hash := desiredHash(normalize(desired))
	name, err := formatModernName(projectSlug, key, hash, marker)
	if err != nil {
		return Observed{}, err
	}
	workflows, err := a.findWorkflowsByName(ctx, organization, name)
	if err != nil {
		return Observed{}, err
	}
	detectors, err := a.findDetectorsByName(ctx, organization, projectSlug, name)
	if err != nil {
		return Observed{}, err
	}
	if len(workflows) > 1 || len(detectors) > 1 || (len(detectors) == 1 && len(workflows) == 0) {
		return Observed{}, ErrModernInvalidState
	}

	var workflow modernWorkflowResponse
	if len(workflows) == 0 {
		workflow, err = a.createWorkflow(ctx, organization, name, desired)
		if err != nil {
			return Observed{}, err
		}
	} else {
		workflow = workflows[0]
	}
	if len(detectors) == 0 {
		var detector modernDetectorResponse
		detector, err = a.createDetector(ctx, organization, projectSlug, name, workflow.ID, desired)
		if err != nil {
			return Observed{}, err
		}
		detectors = []modernDetectorResponse{detector}
	}
	identity := modernIdentity{Detector: detectors[0].ID, Workflow: workflow.ID}
	// Sentry 26 may accept enabled=false on detector creation but still return an
	// enabled detector. Converge both children through the live-confirmed toggle
	// routes before verification so a disabled alert is never reported ready
	// while one half of the pair remains active.
	if detectors[0].Enabled != desired.Enabled || workflow.Enabled != desired.Enabled {
		if err := a.putEnabled(ctx, organization, "workflows", identity.Workflow, desired.Enabled); err != nil {
			return Observed{}, err
		}
		if err := a.putEnabled(ctx, organization, "detectors", identity.Detector, desired.Enabled); err != nil {
			return Observed{}, err
		}
	}
	encoded, err := encodeModernIdentity(identity)
	if err != nil {
		return Observed{}, err
	}
	observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, encoded)
	if err != nil || !found {
		return Observed{}, errors.Join(ErrModernInvalidState, err)
	}
	return observed, nil
}

func (a *ModernHTTPAdapter) Observe(ctx context.Context, adapter, organization, projectSlug, identityValue string) (Observed, bool, error) {
	if a.Client == nil || adapter != ModernMetricIssueAdapter {
		return Observed{}, false, ErrModernInvalidState
	}
	identity, err := parseModernIdentity(identityValue)
	if err != nil {
		return Observed{}, false, err
	}
	detector, detectorFound, err := a.getDetector(ctx, organization, projectSlug, identity.Detector)
	if err != nil {
		return Observed{}, false, err
	}
	workflow, workflowFound, err := a.getWorkflow(ctx, organization, identity.Workflow)
	if err != nil {
		return Observed{}, false, err
	}
	if !detectorFound && !workflowFound {
		return Observed{}, false, nil
	}
	observed, err := observedFromModern(identity, optionalDetector(detector, detectorFound), optionalWorkflow(workflow, workflowFound), projectSlug)
	return observed, true, err
}

func (a *ModernHTTPAdapter) Update(ctx context.Context, adapter, organization, projectSlug, identityValue string, desired Desired) (Observed, error) {
	if err := a.validate(adapter, projectSlug, "rule", desired); err != nil {
		return Observed{}, err
	}
	identity, current, err := a.requirePair(ctx, organization, projectSlug, identityValue)
	if err != nil {
		return Observed{}, err
	}
	parsed, ok := parseModernName(current.detector.Name)
	if !ok || parsed.Project != projectSlug {
		return Observed{}, ErrModernInvalidState
	}
	name, err := formatModernName(projectSlug, parsed.Key, desiredHash(normalize(desired)), parsed.Marker)
	if err != nil {
		return Observed{}, err
	}
	workflow, err := a.putWorkflow(ctx, organization, identity.Workflow, name, desired)
	if err != nil {
		return Observed{}, err
	}
	detector, err := a.putDetector(ctx, organization, identity.Detector, name, identity.Workflow, desired)
	if err != nil {
		return Observed{}, err
	}
	return observedFromModern(identity, &detector, &workflow, projectSlug)
}

func (a *ModernHTTPAdapter) SetEnabled(ctx context.Context, adapter, organization, projectSlug, identityValue string, enabled bool) (Observed, error) {
	if a.Client == nil || adapter != ModernMetricIssueAdapter {
		return Observed{}, ErrModernInvalidState
	}
	identity, _, err := a.requirePair(ctx, organization, projectSlug, identityValue)
	if err != nil {
		return Observed{}, err
	}
	if err := a.putEnabled(ctx, organization, "workflows", identity.Workflow, enabled); err != nil {
		return Observed{}, err
	}
	if err := a.putEnabled(ctx, organization, "detectors", identity.Detector, enabled); err != nil {
		return Observed{}, err
	}
	observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, identityValue)
	if err != nil || !found {
		return Observed{}, errors.Join(ErrModernInvalidState, err)
	}
	return observed, nil
}

func (a *ModernHTTPAdapter) Delete(ctx context.Context, adapter, organization, projectSlug, identityValue string, marker binding.Marker) error {
	if a.Client == nil || adapter != ModernMetricIssueAdapter {
		return ErrModernInvalidState
	}
	identity, err := parseModernIdentity(identityValue)
	if err != nil {
		return err
	}
	// Observe first so an arbitrary protected identity cannot be used to delete a foreign pair.
	observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, identityValue)
	if err != nil || !found || observed.Identity != identityValue || !observed.Marker.Matches(marker) {
		return errors.Join(ErrModernInvalidState, err)
	}
	if err := a.deleteOne(ctx, organization, "detectors", identity.Detector); err != nil {
		return err
	}
	return a.deleteOne(ctx, organization, "workflows", identity.Workflow)
}

type modernPair struct {
	detector modernDetectorResponse
	workflow modernWorkflowResponse
}

func (a *ModernHTTPAdapter) requirePair(ctx context.Context, organization, projectSlug, identityValue string) (modernIdentity, modernPair, error) {
	identity, err := parseModernIdentity(identityValue)
	if err != nil {
		return modernIdentity{}, modernPair{}, err
	}
	detector, detectorFound, err := a.getDetector(ctx, organization, projectSlug, identity.Detector)
	if err != nil || !detectorFound {
		return modernIdentity{}, modernPair{}, errors.Join(ErrModernInvalidState, err)
	}
	workflow, workflowFound, err := a.getWorkflow(ctx, organization, identity.Workflow)
	if err != nil || !workflowFound {
		return modernIdentity{}, modernPair{}, errors.Join(ErrModernInvalidState, err)
	}
	if _, err := observedFromModern(identity, &detector, &workflow, projectSlug); err != nil {
		return modernIdentity{}, modernPair{}, err
	}
	return identity, modernPair{detector: detector, workflow: workflow}, nil
}

func (a *ModernHTTPAdapter) validate(adapter, projectSlug, key string, desired Desired) error {
	_, metricTrigger := metricAggregate(desired.Trigger)
	if a.Client == nil || adapter != ModernMetricIssueAdapter || !metricTrigger || desired.WindowMinutes < 1 || desired.WindowMinutes > 1440 || desired.Threshold <= 0 || !dnsLabel(projectSlug) || !dnsLabel(key) {
		return ErrModernInvalidState
	}
	_, err := a.actions(desired.Notify)
	return err
}

func (a *ModernHTTPAdapter) actions(names []string) ([]modernActionFilter, error) {
	actions := make([]modernAction, 0, len(names))
	for _, name := range names {
		destination, ok := a.Destinations[name]
		if !ok || destination.State != "Active" {
			return nil, ErrModernDestinationUnsupported
		}
		if destination.Kind == "telegram" {
			if destination.Telegram == nil || destination.Telegram.RouteKey == "" || destination.Telegram.Target == nil || destination.Telegram.RouteRevoked {
				return nil, ErrModernDestinationUnsupported
			}
			continue
		}
		if destination.IntegrationIdentity == "" || destination.ResourceIdentity == "" || destination.ResourceName == "" {
			return nil, ErrModernDestinationUnsupported
		}
		integrationID, err := strconv.ParseInt(destination.IntegrationIdentity, 10, 64)
		if err != nil || integrationID <= 0 {
			return nil, ErrModernDestinationUnsupported
		}
		if destination.Kind != "slack" && destination.Kind != "pagerduty" {
			return nil, ErrModernDestinationUnsupported
		}
		actions = append(actions, modernAction{
			Type: destination.Kind, IntegrationID: integrationID, Data: map[string]any{},
			Config: map[string]any{"targetType": "specific", "targetDisplay": destination.ResourceName, "targetIdentifier": destination.ResourceIdentity},
		})
	}
	if len(actions) == 0 {
		return []modernActionFilter{}, nil
	}
	return []modernActionFilter{{LogicType: "all", Conditions: []modernCondition{}, Actions: actions}}, nil
}

func (a *ModernHTTPAdapter) workflowPayload(name string, desired Desired) (modernWorkflowPayload, error) {
	actionFilters, err := a.actions(desired.Notify)
	if err != nil {
		return modernWorkflowPayload{}, err
	}
	return modernWorkflowPayload{
		Name: name, Enabled: desired.Enabled, Config: map[string]any{},
		Triggers: modernConditionGroup{LogicType: "any", Conditions: []modernCondition{}}, ActionFilters: actionFilters,
	}, nil
}

func detectorPayload(name, workflowID string, desired Desired) modernDetectorPayload {
	aggregate, _ := metricAggregate(desired.Trigger)
	return modernDetectorPayload{
		Name: name, Type: "metric_issue", Enabled: desired.Enabled,
		DataSources: []modernDataSource{{QueryType: 0, Dataset: "events", Query: "", Aggregate: aggregate, TimeWindow: desired.WindowMinutes * 60, Environment: nil, EventTypes: []string{"error"}}},
		ConditionGroup: modernConditionGroup{LogicType: "any", Conditions: []modernCondition{
			{Type: "gt", Comparison: desired.Threshold, ConditionResult: 75},
			{Type: "lte", Comparison: 0, ConditionResult: 0},
		}},
		Config: map[string]any{"thresholdPeriod": 1, "detectionType": "static"}, WorkflowIDs: []string{workflowID},
	}
}

func (a *ModernHTTPAdapter) createWorkflow(ctx context.Context, organization, name string, desired Desired) (modernWorkflowResponse, error) {
	payload, err := a.workflowPayload(name, desired)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	var response modernWorkflowResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: organizationPath(organization) + "workflows/", Body: payload, Operation: "create-modern-workflow"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) putWorkflow(ctx context.Context, organization, identity, name string, desired Desired) (modernWorkflowResponse, error) {
	payload, err := a.workflowPayload(name, desired)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	var response modernWorkflowResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: organizationPath(organization) + "workflows/" + url.PathEscape(identity) + "/", Body: payload, Operation: "update-modern-workflow"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) createDetector(ctx context.Context, organization, projectSlug, name, workflowID string, desired Desired) (modernDetectorResponse, error) {
	var response modernDetectorResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: organizationPath(organization) + "projects/" + url.PathEscape(projectSlug) + "/detectors/", Body: detectorPayload(name, workflowID, desired), Operation: "create-modern-detector"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) putDetector(ctx context.Context, organization, identity, name, workflowID string, desired Desired) (modernDetectorResponse, error) {
	var response modernDetectorResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: organizationPath(organization) + "detectors/" + url.PathEscape(identity) + "/", Body: detectorPayload(name, workflowID, desired), Operation: "update-modern-detector"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) findWorkflowsByName(ctx context.Context, organization, name string) ([]modernWorkflowResponse, error) {
	items, err := sentryapi.CollectPages[modernWorkflowResponse](ctx, a.Client, organizationPath(organization)+"workflows/", "list-modern-workflows")
	return matchingWorkflows(items, name), err
}

func (a *ModernHTTPAdapter) findDetectorsByName(ctx context.Context, organization, projectSlug, name string) ([]modernDetectorResponse, error) {
	path := organizationPath(organization) + "detectors/?project=" + url.QueryEscape(projectSlug) + "&type=metric_issue"
	items, err := sentryapi.CollectPages[modernDetectorResponse](ctx, a.Client, path, "list-modern-detectors")
	return matchingDetectors(items, name), err
}

func (a *ModernHTTPAdapter) getWorkflow(ctx context.Context, organization, identity string) (modernWorkflowResponse, bool, error) {
	var response modernWorkflowResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: organizationPath(organization) + "workflows/" + url.PathEscape(identity) + "/", RetrySafe: true, Operation: "get-modern-workflow"}, &response)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return modernWorkflowResponse{}, false, nil
	}
	return response, err == nil, err
}

func (a *ModernHTTPAdapter) getDetector(ctx context.Context, organization, projectSlug, identity string) (modernDetectorResponse, bool, error) {
	path := organizationPath(organization) + "detectors/?id=" + url.QueryEscape(identity) + "&project=" + url.QueryEscape(projectSlug)
	items, err := sentryapi.CollectPages[modernDetectorResponse](ctx, a.Client, path, "get-modern-detector")
	if err != nil {
		return modernDetectorResponse{}, false, err
	}
	if len(items) == 0 {
		return modernDetectorResponse{}, false, nil
	}
	if len(items) != 1 || items[0].ID != identity {
		return modernDetectorResponse{}, false, ErrModernInvalidState
	}
	return items[0], true, nil
}

func (a *ModernHTTPAdapter) putEnabled(ctx context.Context, organization, resource, identity string, enabled bool) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{
		Method: http.MethodPut, Path: organizationPath(organization) + resource + "/?id=" + url.QueryEscape(identity),
		Body: struct {
			Enabled bool `json:"enabled"`
		}{Enabled: enabled}, Operation: "toggle-modern-" + resource,
	}, nil)
	return err
}

func (a *ModernHTTPAdapter) deleteOne(ctx context.Context, organization, resource, identity string) error {
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodDelete, Path: organizationPath(organization) + resource + "/" + url.PathEscape(identity) + "/", Operation: "delete-modern-" + resource}, nil)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return nil
	}
	return err
}

func observedFromModern(identity modernIdentity, detector *modernDetectorResponse, workflow *modernWorkflowResponse, projectSlug string) (Observed, error) {
	if detector == nil && workflow == nil {
		return Observed{}, ErrModernInvalidState
	}
	var authoritative modernName
	var enabled bool
	if detector != nil {
		parsed, ok := parseModernName(detector.Name)
		if !ok || detector.ID != identity.Detector || detector.Type != "metric_issue" || parsed.Project != projectSlug {
			return Observed{}, ErrModernInvalidState
		}
		authoritative, enabled = parsed, detector.Enabled
	}
	if workflow != nil {
		parsed, ok := parseModernName(workflow.Name)
		if !ok || workflow.ID != identity.Workflow || parsed.Project != projectSlug {
			return Observed{}, ErrModernInvalidState
		}
		if detector == nil {
			authoritative, enabled = parsed, workflow.Enabled
		} else {
			if parsed.Key != authoritative.Key || parsed.Marker != authoritative.Marker || !containsID(detector.WorkflowIDs, workflow.ID) || !containsID(workflow.DetectorIDs, detector.ID) {
				return Observed{}, ErrModernInvalidState
			}
			// Detector is authoritative during workflow-first update/toggle recovery.
		}
	}
	encoded, err := encodeModernIdentity(identity)
	if err != nil {
		return Observed{}, err
	}
	return Observed{Identity: encoded, Adapter: ModernMetricIssueAdapter, Marker: authoritative.Marker, DesiredHash: authoritative.Hash, Enabled: enabled}, nil
}

func formatModernName(projectSlug, key, hash string, marker binding.Marker) (string, error) {
	name := fmt.Sprintf("sko:%s:%s:%s [managed;o=%s;m=%s]", projectSlug, key, hash, marker.Owner, marker.ManagedBy)
	parsed, ok := parseModernName(name)
	if !ok || parsed.Marker != marker || utf8.RuneCountInString(name) > 256 {
		return "", ErrModernInvalidState
	}
	return name, nil
}

func parseModernName(name string) (modernName, bool) {
	matches := managedAlertNamePattern.FindStringSubmatch(name)
	if len(matches) != 6 {
		return modernName{}, false
	}
	return modernName{Project: matches[1], Key: matches[2], Hash: matches[3], Marker: binding.Marker{Origin: "managed", Owner: matches[4], ManagedBy: matches[5]}}, true
}

func encodeModernIdentity(identity modernIdentity) (string, error) {
	if !numericIdentityPattern.MatchString(identity.Detector) || !numericIdentityPattern.MatchString(identity.Workflow) {
		return "", ErrModernInvalidState
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return "", ErrModernInvalidState
	}
	return string(payload), nil
}

func parseModernIdentity(value string) (modernIdentity, error) {
	var identity modernIdentity
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil || !numericIdentityPattern.MatchString(identity.Detector) || !numericIdentityPattern.MatchString(identity.Workflow) {
		return modernIdentity{}, ErrModernInvalidState
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return modernIdentity{}, ErrModernInvalidState
	}
	return identity, nil
}

func organizationPath(organization string) string {
	return "/api/0/organizations/" + url.PathEscape(organization) + "/"
}

func matchingWorkflows(items []modernWorkflowResponse, name string) []modernWorkflowResponse {
	result := make([]modernWorkflowResponse, 0, 1)
	for _, item := range items {
		if item.Name == name {
			result = append(result, item)
		}
	}
	return result
}

func matchingDetectors(items []modernDetectorResponse, name string) []modernDetectorResponse {
	result := make([]modernDetectorResponse, 0, 1)
	for _, item := range items {
		if item.Name == name {
			result = append(result, item)
		}
	}
	return result
}

func optionalDetector(value modernDetectorResponse, found bool) *modernDetectorResponse {
	if found {
		return &value
	}
	return nil
}
func optionalWorkflow(value modernWorkflowResponse, found bool) *modernWorkflowResponse {
	if found {
		return &value
	}
	return nil
}
func containsID(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func dnsLabel(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for index, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9' && index > 0) || (r == '-' && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}
