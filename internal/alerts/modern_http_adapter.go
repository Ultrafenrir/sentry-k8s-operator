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
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

const ModernMetricIssueAdapter = "modern-metric-issue-v1"

var (
	ErrModernInvalidState           = errors.New("modern metric alert state is invalid")
	ErrModernDestinationUnsupported = errors.New("modern metric alert destination is unsupported")
	ErrModernDynamicUnavailable     = errors.New("dynamic metric detection backend is unavailable")
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
	Detector       string `json:"detector"`
	Workflow       string `json:"workflow"`
	SharedDetector bool   `json:"sharedDetector,omitempty"`
	SharedWorkflow bool   `json:"sharedWorkflow,omitempty"`
}

type modernName struct {
	Project string
	Key     string
	Hash    string
	Marker  binding.Marker
}

type modernWorkflowResponse struct {
	ID            string                       `json:"id"`
	Name          string                       `json:"name"`
	Enabled       bool                         `json:"enabled"`
	Config        map[string]any               `json:"config"`
	Triggers      modernConditionGroup         `json:"triggers"`
	ActionFilters []modernActionFilterResponse `json:"actionFilters"`
	Environment   *string                      `json:"environment"`
	DetectorIDs   []string                     `json:"detectorIds"`
}

type modernDetectorResponse struct {
	ID             string                       `json:"id"`
	ProjectID      string                       `json:"projectId"`
	Name           string                       `json:"name"`
	Type           string                       `json:"type"`
	Enabled        bool                         `json:"enabled"`
	Config         map[string]any               `json:"config"`
	ConditionGroup *modernConditionGroup        `json:"conditionGroup"`
	DataSources    []modernDataSourceResponse   `json:"dataSources"`
	WorkflowIDs    []string                     `json:"workflowIds"`
	Description    string                       `json:"description"`
	Owner          *modernDetectorOwnerResponse `json:"owner"`
}

type modernDetectorOwnerResponse struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type modernDataSourceResponse struct {
	QueryObj struct {
		SnubaQuery struct {
			Dataset     string   `json:"dataset"`
			Query       string   `json:"query"`
			Aggregate   string   `json:"aggregate"`
			TimeWindow  int      `json:"timeWindow"`
			Environment *string  `json:"environment"`
			EventTypes  []string `json:"eventTypes"`
		} `json:"snubaQuery"`
	} `json:"queryObj"`
}

type modernCondition struct {
	Type            string `json:"type"`
	Comparison      any    `json:"comparison"`
	ConditionResult any    `json:"conditionResult"`
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

func (a modernAction) MarshalJSON() ([]byte, error) {
	type wireAction modernAction
	integrationID := any(a.IntegrationID)
	if a.IntegrationID == 0 {
		integrationID = nil
	}
	return json.Marshal(struct {
		wireAction
		IntegrationID any `json:"integrationId"`
	}{wireAction: wireAction(a), IntegrationID: integrationID})
}

type modernActionFilter struct {
	LogicType  string            `json:"logicType"`
	Conditions []modernCondition `json:"conditions"`
	Actions    []modernAction    `json:"actions"`
}

type modernActionResponse struct {
	Type          string         `json:"type"`
	IntegrationID any            `json:"integrationId"`
	Data          map[string]any `json:"data"`
	Config        map[string]any `json:"config"`
}

type modernActionFilterResponse struct {
	LogicType  string                 `json:"logicType"`
	Conditions []modernCondition      `json:"conditions"`
	Actions    []modernActionResponse `json:"actions"`
}

type modernWorkflowPayload struct {
	Name          string               `json:"name"`
	Enabled       bool                 `json:"enabled"`
	Config        map[string]any       `json:"config"`
	Triggers      modernConditionGroup `json:"triggers"`
	ActionFilters []modernActionFilter `json:"actionFilters"`
	Environment   *string              `json:"environment"`
	DetectorIDs   []string             `json:"detectorIds,omitempty"`
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
	Description    string               `json:"description,omitempty"`
	Owner          *string              `json:"owner,omitempty"`
}

func (a *ModernHTTPAdapter) Ensure(ctx context.Context, adapter, organization, projectSlug, key string, desired Desired, marker binding.Marker) (Observed, error) {
	if err := a.validate(adapter, projectSlug, key, desired); err != nil {
		return Observed{}, err
	}
	if desired.Metric != nil && desired.Metric.Detection.Kind == "dynamic" {
		if err := a.probeDynamicCapability(ctx); err != nil {
			return Observed{}, err
		}
	}
	hash := desiredHash(normalize(desired))
	name, err := formatModernName(projectSlug, key, hash, marker)
	if err != nil {
		return Observed{}, err
	}
	if desired.Issue != nil {
		return a.ensureIssue(ctx, adapter, organization, projectSlug, name, desired)
	}
	workflowName, err := formatModernName(projectSlug, key, modernWorkflowHash(desired), marker)
	if err != nil {
		return Observed{}, err
	}
	detectors, err := a.findDetectorsByName(ctx, organization, projectSlug, name)
	if err != nil {
		return Observed{}, err
	}
	if len(detectors) > 1 {
		return Observed{}, ErrModernInvalidState
	}
	var workflow modernWorkflowResponse
	if len(detectors) == 1 {
		if len(detectors[0].WorkflowIDs) != 1 {
			return Observed{}, ErrModernInvalidState
		}
		var found bool
		workflow, found, err = a.getWorkflow(ctx, organization, detectors[0].WorkflowIDs[0])
		if err != nil || !found {
			return Observed{}, errors.Join(ErrModernInvalidState, err)
		}
	} else {
		workflow, err = a.findReusableWorkflow(ctx, organization, projectSlug, workflowName, desired, "")
		if err != nil {
			return Observed{}, err
		}
		if workflow.ID == "" {
			workflow, err = a.createWorkflow(ctx, organization, workflowName, desired, nil)
			if err != nil {
				return Observed{}, err
			}
		}
	}
	if len(detectors) == 0 {
		var detector modernDetectorResponse
		detector, err = a.createDetector(ctx, organization, projectSlug, name, workflow.ID, desired)
		if err != nil {
			return Observed{}, err
		}
		detectors = []modernDetectorResponse{detector}
	}
	if fresh, found, fetchErr := a.getWorkflow(ctx, organization, workflow.ID); fetchErr != nil {
		return Observed{}, fetchErr
	} else if found {
		workflow = fresh
	} else {
		return Observed{}, ErrModernInvalidState
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
	if err := a.verifyMetric(ctx, organization, projectSlug, identity, name, desired); err != nil {
		return Observed{}, err
	}
	return observed, nil
}

func (a *ModernHTTPAdapter) probeDynamicCapability(ctx context.Context) error {
	var response struct {
		Models []string `json:"models"`
	}
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: "/api/0/seer/models/", RetrySafe: true, Operation: "probe-dynamic-detection"}, &response)
	if err != nil || response.Models == nil {
		return errors.Join(ErrModernDynamicUnavailable, err)
	}
	return nil
}

func (a *ModernHTTPAdapter) ensureIssue(ctx context.Context, adapter, organization, projectSlug, name string, desired Desired) (Observed, error) {
	detector, err := a.findIssueStream(ctx, organization, projectSlug)
	if err != nil {
		return Observed{}, err
	}
	workflows, err := a.findWorkflowsByName(ctx, organization, name)
	if err != nil {
		return Observed{}, err
	}
	if len(workflows) > 1 {
		return Observed{}, ErrModernInvalidState
	}
	var workflow modernWorkflowResponse
	if len(workflows) == 0 {
		workflow, err = a.createWorkflow(ctx, organization, name, desired, []string{detector.ID})
	} else {
		workflow = workflows[0]
		if !containsID(workflow.DetectorIDs, detector.ID) {
			workflow, err = a.putWorkflow(ctx, organization, workflow.ID, name, desired, []string{detector.ID})
		}
	}
	if err != nil {
		return Observed{}, err
	}
	identity := modernIdentity{Detector: detector.ID, Workflow: workflow.ID, SharedDetector: true}
	if workflow.Enabled != desired.Enabled {
		if err := a.putEnabled(ctx, organization, "workflows", workflow.ID, desired.Enabled); err != nil {
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
	if err := a.verifyIssue(ctx, organization, projectSlug, identity, name, desired); err != nil {
		return Observed{}, err
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
	if detectorFound && !identity.SharedDetector && (!workflowFound || !containsID(detector.WorkflowIDs, identity.Workflow)) && len(detector.WorkflowIDs) == 1 {
		workflow, workflowFound, err = a.getWorkflow(ctx, organization, detector.WorkflowIDs[0])
		if err != nil {
			return Observed{}, false, err
		}
		if workflowFound {
			identity.Workflow = workflow.ID
		}
	}
	if !detectorFound && !workflowFound {
		return Observed{}, false, nil
	}
	if !identity.SharedDetector && !detectorFound && workflowFound && len(workflow.DetectorIDs) > 0 {
		return Observed{}, false, nil
	}
	observed, err := observedFromModern(identity, optionalDetector(detector, detectorFound), optionalWorkflow(workflow, workflowFound), projectSlug)
	return observed, true, err
}

func (a *ModernHTTPAdapter) Update(ctx context.Context, adapter, organization, projectSlug, identityValue string, desired Desired) (Observed, error) {
	if err := a.validate(adapter, projectSlug, "rule", desired); err != nil {
		return Observed{}, err
	}
	if desired.Metric != nil && desired.Metric.Detection.Kind == "dynamic" {
		if err := a.probeDynamicCapability(ctx); err != nil {
			return Observed{}, err
		}
	}
	identity, current, err := a.requirePair(ctx, organization, projectSlug, identityValue)
	if err != nil {
		return Observed{}, err
	}
	authoritativeName := current.detector.Name
	if identity.SharedDetector {
		authoritativeName = current.workflow.Name
	}
	parsed, ok := parseModernName(authoritativeName)
	if !ok || parsed.Project != projectSlug {
		return Observed{}, ErrModernInvalidState
	}
	name, err := formatModernName(projectSlug, parsed.Key, desiredHash(normalize(desired)), parsed.Marker)
	if err != nil {
		return Observed{}, err
	}
	workflowName, err := formatModernName(projectSlug, parsed.Key, modernWorkflowHash(desired), parsed.Marker)
	if err != nil {
		return Observed{}, err
	}
	if identity.SharedDetector {
		workflowName = name
	}
	workflow := current.workflow
	reusable, err := a.findReusableWorkflow(ctx, organization, projectSlug, workflowName, desired, identity.Workflow)
	if err != nil {
		return Observed{}, err
	}
	if reusable.ID != "" {
		workflow = reusable
	} else if len(current.workflow.DetectorIDs) > 1 {
		workflow, err = a.createWorkflow(ctx, organization, workflowName, desired, nil)
		if err != nil {
			return Observed{}, err
		}
	} else {
		workflow, err = a.putWorkflow(ctx, organization, identity.Workflow, workflowName, desired, []string{identity.Detector})
		if err != nil {
			return Observed{}, err
		}
	}
	if identity.SharedDetector {
		if err := a.verifyIssue(ctx, organization, projectSlug, identity, name, desired); err != nil {
			return Observed{}, err
		}
		return observedFromModern(identity, &current.detector, &workflow, projectSlug)
	}
	detector, err := a.putDetector(ctx, organization, identity.Detector, name, workflow.ID, desired)
	if err != nil {
		return Observed{}, err
	}
	if workflow.ID != identity.Workflow {
		if err := a.deleteWorkflowIfUnlinked(ctx, organization, identity.Workflow); err != nil {
			return Observed{}, err
		}
		identity.Workflow = workflow.ID
	}
	freshWorkflow, found, err := a.getWorkflow(ctx, organization, identity.Workflow)
	if err != nil || !found {
		return Observed{}, errors.Join(ErrModernInvalidState, err)
	}
	workflow = freshWorkflow
	if err := a.verifyMetric(ctx, organization, projectSlug, identity, name, desired); err != nil {
		return Observed{}, err
	}
	return observedFromModern(identity, &detector, &workflow, projectSlug)
}

func (a *ModernHTTPAdapter) SetEnabled(ctx context.Context, adapter, organization, projectSlug, identityValue string, enabled bool) (Observed, error) {
	if a.Client == nil || adapter != ModernMetricIssueAdapter {
		return Observed{}, ErrModernInvalidState
	}
	pairIdentity, pair, err := a.requirePair(ctx, organization, projectSlug, identityValue)
	if err != nil {
		return Observed{}, err
	}
	identity := pairIdentity
	if !identity.SharedDetector && len(pair.workflow.DetectorIDs) > 1 {
		newWorkflow, splitErr := a.cloneWorkflowForDetector(ctx, organization, pair.workflow, pair.detector, enabled)
		if splitErr != nil {
			return Observed{}, splitErr
		}
		identity.Workflow = newWorkflow.ID
		if err := a.deleteWorkflowIfUnlinked(ctx, organization, pair.workflow.ID); err != nil {
			return Observed{}, err
		}
	} else if err := a.putEnabled(ctx, organization, "workflows", identity.Workflow, enabled); err != nil {
		return Observed{}, err
	}
	if identity.SharedDetector {
		observed, found, err := a.Observe(ctx, adapter, organization, projectSlug, identityValue)
		if err != nil || !found {
			return Observed{}, errors.Join(ErrModernInvalidState, err)
		}
		return observed, nil
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
	if !identity.SharedDetector {
		if err := a.deleteOne(ctx, organization, "detectors", identity.Detector); err != nil {
			return err
		}
		return a.deleteWorkflowIfUnlinked(ctx, organization, identity.Workflow)
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
	metric := desired.Metric != nil
	issue := desired.Issue != nil
	_, compatibilityMetric := metricAggregate(desired.Trigger)
	if a.Client == nil || adapter != ModernMetricIssueAdapter || !dnsLabel(projectSlug) || !dnsLabel(key) || (!metric && !issue && !compatibilityMetric) {
		return ErrModernInvalidState
	}
	if compatibilityMetric && !metric && (desired.WindowMinutes < 1 || desired.WindowMinutes > 1440 || desired.Threshold <= 0) {
		return ErrModernInvalidState
	}
	for _, name := range destinationReferences(desired) {
		if _, err := a.destinationAction(name, ""); err != nil {
			return err
		}
	}
	return nil
}

func (a *ModernHTTPAdapter) destinationAction(name, severity string) (modernAction, error) {
	destination, ok := a.Destinations[name]
	if !ok || destination.State != "Active" || destination.IntegrationIdentity == "" || destination.ResourceIdentity == "" || destination.ResourceName == "" {
		return modernAction{}, ErrModernDestinationUnsupported
	}
	integrationID, err := strconv.ParseInt(destination.IntegrationIdentity, 10, 64)
	if err != nil || integrationID <= 0 || destination.Kind != "slack" && destination.Kind != "pagerduty" {
		return modernAction{}, ErrModernDestinationUnsupported
	}
	data := map[string]any{}
	if destination.Kind == "pagerduty" {
		if severity == "" {
			severity = "default"
		}
		data["priority"] = severity
	}
	return modernAction{
		Type: destination.Kind, IntegrationID: integrationID, Data: data,
		Config: map[string]any{"targetType": "specific", "targetDisplay": destination.ResourceName, "targetIdentifier": destination.ResourceIdentity},
	}, nil
}

func (a *ModernHTTPAdapter) workflowPayload(ctx context.Context, organization, name string, desired Desired, detectorIDs []string) (modernWorkflowPayload, error) {
	actionFilters, err := a.actionFilters(ctx, organization, desired)
	if err != nil {
		return modernWorkflowPayload{}, err
	}
	triggers, err := modernWorkflowTriggers(desired)
	if err != nil {
		return modernWorkflowPayload{}, err
	}
	var environment *string
	if desired.Issue != nil && desired.Issue.Environment != "" {
		environment = &desired.Issue.Environment
	}
	return modernWorkflowPayload{
		Name: name, Enabled: desired.Enabled, Config: map[string]any{"frequency": desired.ThrottleMinutes},
		Triggers: triggers, ActionFilters: actionFilters, Environment: environment, DetectorIDs: detectorIDs,
	}, nil
}

func detectorPayload(name, workflowID string, desired Desired) modernDetectorPayload {
	payload, _ := (&ModernHTTPAdapter{}).detectorPayloadContext(context.Background(), "", name, workflowID, desired)
	return payload
}

func (a *ModernHTTPAdapter) detectorPayloadContext(ctx context.Context, organization, name, workflowID string, desired Desired) (modernDetectorPayload, error) {
	dataset, aggregate, query, eventTypes, err := modernMetricSource(desired)
	if err != nil {
		return modernDetectorPayload{}, err
	}
	interval := desired.WindowMinutes
	if desired.Metric != nil {
		interval = int(desired.Metric.IntervalMinutes)
	}
	conditions, config := modernDetection(desired)
	var environment *string
	if desired.Metric != nil && desired.Metric.Environment != "" {
		environment = &desired.Metric.Environment
	}
	payload := modernDetectorPayload{
		Name: name, Type: "metric_issue", Enabled: desired.Enabled,
		DataSources:    []modernDataSource{{QueryType: 0, Dataset: dataset, Query: query, Aggregate: aggregate, TimeWindow: interval * 60, Environment: environment, EventTypes: eventTypes}},
		ConditionGroup: modernConditionGroup{LogicType: "any", Conditions: conditions},
		Config:         config, WorkflowIDs: []string{workflowID}, Description: desired.Description,
	}
	if dataset == "events_analytics_platform" {
		payload.DataSources[0].QueryType = 1
	}
	if desired.OwnerTeam != "" {
		teamID, err := a.resolveTeamID(ctx, organization, desired.OwnerTeam)
		if err != nil {
			return modernDetectorPayload{}, err
		}
		owner := "team:" + teamID
		payload.Owner = &owner
	}
	return payload, nil
}

func (a *ModernHTTPAdapter) createWorkflow(ctx context.Context, organization, name string, desired Desired, detectorIDs []string) (modernWorkflowResponse, error) {
	payload, err := a.workflowPayload(ctx, organization, name, desired, detectorIDs)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	var response modernWorkflowResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: organizationPath(organization) + "workflows/", Body: payload, Operation: "create-modern-workflow"}, &response)
	if err != nil {
		matches, findErr := a.findWorkflowsByName(ctx, organization, name)
		if findErr == nil && len(matches) == 1 {
			actual, found, getErr := a.getWorkflow(ctx, organization, matches[0].ID)
			if getErr == nil && found && workflowMatches(payload, actual) {
				return actual, nil
			}
		}
		return modernWorkflowResponse{}, errors.Join(err, findErr)
	}
	return response, err
}

func (a *ModernHTTPAdapter) putWorkflow(ctx context.Context, organization, identity, name string, desired Desired, detectorIDs []string) (modernWorkflowResponse, error) {
	payload, err := a.workflowPayload(ctx, organization, name, desired, detectorIDs)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	var response modernWorkflowResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: organizationPath(organization) + "workflows/" + url.PathEscape(identity) + "/", Body: payload, Operation: "update-modern-workflow"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) createDetector(ctx context.Context, organization, projectSlug, name, workflowID string, desired Desired) (modernDetectorResponse, error) {
	payload, err := a.detectorPayloadContext(ctx, organization, name, workflowID, desired)
	if err != nil {
		return modernDetectorResponse{}, err
	}
	var response modernDetectorResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: organizationPath(organization) + "projects/" + url.PathEscape(projectSlug) + "/detectors/", Body: payload, Operation: "create-modern-detector"}, &response)
	if err != nil {
		matches, findErr := a.findDetectorsByName(ctx, organization, projectSlug, name)
		if findErr == nil && len(matches) == 1 {
			actual, found, getErr := a.getDetector(ctx, organization, projectSlug, matches[0].ID)
			if getErr == nil && found && detectorMatches(payload, actual) {
				return actual, nil
			}
		}
		return modernDetectorResponse{}, errors.Join(err, findErr)
	}
	return response, err
}

func (a *ModernHTTPAdapter) putDetector(ctx context.Context, organization, identity, name, workflowID string, desired Desired) (modernDetectorResponse, error) {
	payload, err := a.detectorPayloadContext(ctx, organization, name, workflowID, desired)
	if err != nil {
		return modernDetectorResponse{}, err
	}
	var response modernDetectorResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: organizationPath(organization) + "detectors/" + url.PathEscape(identity) + "/", Body: payload, Operation: "update-modern-detector"}, &response)
	return response, err
}

func (a *ModernHTTPAdapter) findWorkflowsByName(ctx context.Context, organization, name string) ([]modernWorkflowResponse, error) {
	items, err := sentryapi.CollectPages[modernWorkflowResponse](ctx, a.Client, organizationPath(organization)+"workflows/", "list-modern-workflows")
	return matchingWorkflows(items, name), err
}

func (a *ModernHTTPAdapter) findReusableWorkflow(ctx context.Context, organization, projectSlug, desiredName string, desired Desired, excludeIdentity string) (modernWorkflowResponse, error) {
	parsedDesired, ok := parseModernName(desiredName)
	if !ok {
		return modernWorkflowResponse{}, ErrModernInvalidState
	}
	items, err := sentryapi.CollectPages[modernWorkflowResponse](ctx, a.Client, organizationPath(organization)+"workflows/", "list-modern-workflows")
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	matches := make([]modernWorkflowResponse, 0, 1)
	for _, item := range items {
		if item.ID == excludeIdentity {
			continue
		}
		parsed, valid := parseModernName(item.Name)
		if !valid || parsed.Project != projectSlug || parsed.Hash != parsedDesired.Hash || parsed.Marker != parsedDesired.Marker {
			continue
		}
		expected, payloadErr := a.workflowPayload(ctx, organization, item.Name, desired, item.DetectorIDs)
		if payloadErr != nil {
			return modernWorkflowResponse{}, payloadErr
		}
		if workflowMatches(expected, item) {
			matches = append(matches, item)
		}
	}
	if len(matches) > 1 {
		return modernWorkflowResponse{}, ErrModernInvalidState
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return modernWorkflowResponse{}, nil
}

func (a *ModernHTTPAdapter) deleteWorkflowIfUnlinked(ctx context.Context, organization, identity string) error {
	workflow, found, err := a.getWorkflow(ctx, organization, identity)
	if err != nil || !found {
		return err
	}
	if len(workflow.DetectorIDs) != 0 {
		return nil
	}
	return a.deleteOne(ctx, organization, "workflows", identity)
}

func (a *ModernHTTPAdapter) cloneWorkflowForDetector(ctx context.Context, organization string, workflow modernWorkflowResponse, detector modernDetectorResponse, enabled bool) (modernWorkflowResponse, error) {
	parsed, ok := parseModernName(detector.Name)
	if !ok {
		return modernWorkflowResponse{}, ErrModernInvalidState
	}
	key := "w" + detector.ID
	name, err := formatModernName(parsed.Project, key, parsed.Hash, parsed.Marker)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	payload, err := workflowResponsePayload(workflow)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	payload.Name = name
	payload.Enabled = enabled
	payload.DetectorIDs = []string{detector.ID}
	created, err := a.createWorkflowPayload(ctx, organization, payload)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	oldPayload, err := workflowResponsePayload(workflow)
	if err != nil {
		return modernWorkflowResponse{}, err
	}
	oldPayload.DetectorIDs = removeID(oldPayload.DetectorIDs, detector.ID)
	if _, err := a.putWorkflowPayload(ctx, organization, workflow.ID, oldPayload); err != nil {
		return modernWorkflowResponse{}, err
	}
	return created, nil
}

func (a *ModernHTTPAdapter) createWorkflowPayload(ctx context.Context, organization string, payload modernWorkflowPayload) (modernWorkflowResponse, error) {
	var response modernWorkflowResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPost, Path: organizationPath(organization) + "workflows/", Body: payload, Operation: "create-modern-workflow"}, &response)
	if err == nil {
		return response, nil
	}
	matches, findErr := a.findWorkflowsByName(ctx, organization, payload.Name)
	if findErr == nil && len(matches) == 1 && workflowMatches(payload, matches[0]) {
		return matches[0], nil
	}
	return modernWorkflowResponse{}, errors.Join(err, findErr)
}

func (a *ModernHTTPAdapter) putWorkflowPayload(ctx context.Context, organization, identity string, payload modernWorkflowPayload) (modernWorkflowResponse, error) {
	var response modernWorkflowResponse
	_, err := a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodPut, Path: organizationPath(organization) + "workflows/" + url.PathEscape(identity) + "/", Body: payload, Operation: "update-modern-workflow"}, &response)
	return response, err
}

func workflowResponsePayload(workflow modernWorkflowResponse) (modernWorkflowPayload, error) {
	actionFilters := make([]modernActionFilter, 0, len(workflow.ActionFilters))
	for _, filter := range workflow.ActionFilters {
		actions := make([]modernAction, 0, len(filter.Actions))
		for _, action := range filter.Actions {
			integrationID := int64(0)
			if action.IntegrationID != nil {
				value, err := strconv.ParseInt(fmt.Sprint(action.IntegrationID), 10, 64)
				if err != nil {
					return modernWorkflowPayload{}, ErrModernInvalidState
				}
				integrationID = value
			}
			actions = append(actions, modernAction{Type: action.Type, IntegrationID: integrationID, Data: action.Data, Config: action.Config})
		}
		actionFilters = append(actionFilters, modernActionFilter{LogicType: filter.LogicType, Conditions: filter.Conditions, Actions: actions})
	}
	return modernWorkflowPayload{Name: workflow.Name, Enabled: workflow.Enabled, Config: workflow.Config, Triggers: workflow.Triggers, ActionFilters: actionFilters, Environment: workflow.Environment, DetectorIDs: append([]string(nil), workflow.DetectorIDs...)}, nil
}

func removeID(values []string, remove string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != remove {
			result = append(result, value)
		}
	}
	return result
}

func (a *ModernHTTPAdapter) findDetectorsByName(ctx context.Context, organization, projectSlug, name string) ([]modernDetectorResponse, error) {
	path := organizationPath(organization) + "detectors/?project=" + url.QueryEscape(projectSlug) + "&type=metric_issue"
	items, err := sentryapi.CollectPages[modernDetectorResponse](ctx, a.Client, path, "list-modern-detectors")
	return matchingDetectors(items, name), err
}

func (a *ModernHTTPAdapter) findIssueStream(ctx context.Context, organization, projectSlug string) (modernDetectorResponse, error) {
	path := organizationPath(organization) + "detectors/?project=" + url.QueryEscape(projectSlug) + "&type=issue_stream"
	items, err := sentryapi.CollectPages[modernDetectorResponse](ctx, a.Client, path, "list-modern-issue-stream")
	if err != nil {
		return modernDetectorResponse{}, err
	}
	if len(items) != 1 || items[0].ID == "" || items[0].Type != "issue_stream" {
		return modernDetectorResponse{}, ErrModernInvalidState
	}
	return items[0], nil
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
	var response modernDetectorResponse
	_, err = a.Client.DoJSON(ctx, sentryapi.Request{Method: http.MethodGet, Path: organizationPath(organization) + "detectors/" + url.PathEscape(identity) + "/", RetrySafe: true, Operation: "get-modern-detector-details"}, &response)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		return modernDetectorResponse{}, false, nil
	}
	if err != nil {
		return modernDetectorResponse{}, false, err
	}
	if response.ID != identity {
		return modernDetectorResponse{}, false, ErrModernInvalidState
	}
	return response, true, nil
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

func (a *ModernHTTPAdapter) verifyIssue(ctx context.Context, organization, projectSlug string, identity modernIdentity, name string, desired Desired) error {
	detector, detectorFound, err := a.getDetector(ctx, organization, projectSlug, identity.Detector)
	if err != nil || !detectorFound || detector.Type != "issue_stream" {
		return errors.Join(ErrModernInvalidState, err)
	}
	workflow, workflowFound, err := a.getWorkflow(ctx, organization, identity.Workflow)
	if err != nil || !workflowFound {
		return errors.Join(ErrModernInvalidState, err)
	}
	payload, err := a.workflowPayload(ctx, organization, name, desired, []string{identity.Detector})
	if err != nil {
		return err
	}
	if !containsID(detector.WorkflowIDs, identity.Workflow) || !workflowMatches(payload, workflow) {
		return ErrModernInvalidState
	}
	return nil
}

func (a *ModernHTTPAdapter) verifyMetric(ctx context.Context, organization, projectSlug string, identity modernIdentity, name string, desired Desired) error {
	detector, detectorFound, err := a.getDetector(ctx, organization, projectSlug, identity.Detector)
	if err != nil || !detectorFound {
		return errors.Join(ErrModernInvalidState, err)
	}
	workflow, workflowFound, err := a.getWorkflow(ctx, organization, identity.Workflow)
	if err != nil || !workflowFound {
		return errors.Join(ErrModernInvalidState, err)
	}
	detectorPayload, err := a.detectorPayloadContext(ctx, organization, name, identity.Workflow, desired)
	if err != nil {
		return err
	}
	workflowPayload, err := a.workflowPayload(ctx, organization, workflow.Name, desired, workflow.DetectorIDs)
	if err != nil {
		return err
	}
	if !detectorMatches(detectorPayload, detector) || !workflowMatches(workflowPayload, workflow) {
		return ErrModernInvalidState
	}
	return nil
}

func workflowMatches(expected modernWorkflowPayload, actual modernWorkflowResponse) bool {
	if expected.Name != actual.Name || expected.Enabled != actual.Enabled || !reflect.DeepEqual(expected.DetectorIDs, actual.DetectorIDs) || !stringPointersEqual(expected.Environment, actual.Environment) {
		return false
	}
	if !jsonValuesEqual(expected.Config, actual.Config) || !conditionGroupsEqual(expected.Triggers, actual.Triggers) || len(expected.ActionFilters) != len(actual.ActionFilters) {
		return false
	}
	for index := range expected.ActionFilters {
		want, got := expected.ActionFilters[index], actual.ActionFilters[index]
		if want.LogicType != got.LogicType || !conditionsEqual(want.Conditions, got.Conditions) || len(want.Actions) != len(got.Actions) {
			return false
		}
		for actionIndex := range want.Actions {
			wantAction, gotAction := want.Actions[actionIndex], got.Actions[actionIndex]
			if wantAction.Type != gotAction.Type || !integrationIDsEqual(wantAction.IntegrationID, gotAction.IntegrationID) || !modernActionDataMatches(wantAction.Type, wantAction.Data, gotAction.Data) || !jsonValuesEqual(wantAction.Config, gotAction.Config) {
				return false
			}
		}
	}
	return true
}

func modernActionDataMatches(actionType string, expected, actual map[string]any) bool {
	if actionType != "github" && actionType != "github_enterprise" {
		return jsonValuesEqual(expected, actual)
	}
	canonical := func(value map[string]any) map[string]any {
		result := map[string]any{}
		if additional, ok := value["additional_fields"]; ok {
			result["additional_fields"] = additional
		} else if additional, ok := value["additionalFields"]; ok {
			result["additional_fields"] = additional
		}
		if fields, ok := value["dynamic_form_fields"]; ok {
			result["dynamic_form_fields"] = fields
		} else if fields, ok := value["dynamicFormFields"]; ok {
			result["dynamic_form_fields"] = fields
		}
		return result
	}
	return jsonValuesEqual(canonical(expected), canonical(actual))
}

func detectorMatches(expected modernDetectorPayload, actual modernDetectorResponse) bool {
	if expected.Name != actual.Name || expected.Type != actual.Type || expected.Enabled != actual.Enabled || expected.Description != actual.Description || !reflect.DeepEqual(expected.WorkflowIDs, actual.WorkflowIDs) || actual.ConditionGroup == nil {
		return false
	}
	if !jsonValuesEqual(expected.Config, actual.Config) || !conditionGroupsEqual(expected.ConditionGroup, *actual.ConditionGroup) || len(expected.DataSources) != len(actual.DataSources) {
		return false
	}
	for index := range expected.DataSources {
		want := expected.DataSources[index]
		got := actual.DataSources[index].QueryObj.SnubaQuery
		if want.Dataset != got.Dataset || want.Query != got.Query || want.Aggregate != got.Aggregate || want.TimeWindow != got.TimeWindow || !stringPointersEqual(want.Environment, got.Environment) || !reflect.DeepEqual(want.EventTypes, got.EventTypes) {
			return false
		}
	}
	if expected.Owner == nil {
		return actual.Owner == nil
	}
	if actual.Owner == nil || actual.Owner.Type+":"+actual.Owner.ID != *expected.Owner {
		return false
	}
	return true
}

func conditionGroupsEqual(expected, actual modernConditionGroup) bool {
	return expected.LogicType == actual.LogicType && conditionsEqual(expected.Conditions, actual.Conditions)
}

func conditionsEqual(expected, actual []modernCondition) bool {
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		if expected[index].Type != actual[index].Type || !jsonValuesEqual(expected[index].Comparison, actual[index].Comparison) || !jsonValuesEqual(expected[index].ConditionResult, actual[index].ConditionResult) {
			return false
		}
	}
	return true
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func integrationIDsEqual(expected int64, actual any) bool {
	if expected == 0 {
		return actual == nil
	}
	return strconv.FormatInt(expected, 10) == fmt.Sprint(actual)
}

func stringPointersEqual(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func observedFromModern(identity modernIdentity, detector *modernDetectorResponse, workflow *modernWorkflowResponse, projectSlug string) (Observed, error) {
	if detector == nil && workflow == nil {
		return Observed{}, ErrModernInvalidState
	}
	var authoritative modernName
	var enabled bool
	if identity.SharedDetector {
		if detector == nil || workflow == nil || detector.ID != identity.Detector || detector.Type != "issue_stream" || workflow.ID != identity.Workflow {
			return Observed{}, ErrModernInvalidState
		}
		parsed, ok := parseModernName(workflow.Name)
		if !ok || parsed.Project != projectSlug || !containsID(detector.WorkflowIDs, workflow.ID) || !containsID(workflow.DetectorIDs, detector.ID) {
			return Observed{}, ErrModernInvalidState
		}
		encoded, err := encodeModernIdentity(identity)
		if err != nil {
			return Observed{}, err
		}
		return Observed{Identity: encoded, Adapter: ModernMetricIssueAdapter, Marker: parsed.Marker, DesiredHash: parsed.Hash, Enabled: workflow.Enabled}, nil
	}
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
			if parsed.Marker != authoritative.Marker || !containsID(detector.WorkflowIDs, workflow.ID) || !containsID(workflow.DetectorIDs, detector.ID) {
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
