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
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestModernHTTPAdapterFullMetricIssueLifecycle(t *testing.T) {
	t.Parallel()

	remote := &modernTestRemote{}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := &ModernHTTPAdapter{
		Client: modernTestClient(t, server),
		Destinations: map[string]binding.DestinationBinding{
			"chat": {Kind: "slack", State: "Active", IntegrationIdentity: "12", ResourceIdentity: "C123", ResourceName: "incidents"},
			"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
		},
	}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"}
	desired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 200, Enabled: true, Notify: []string{"chat", "page"}}

	created, err := adapter.Ensure(context.Background(), ModernMetricIssueAdapter, "company", "checkout", "errors", desired, marker)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if created.Adapter != ModernMetricIssueAdapter || created.Marker != marker || created.DesiredHash != desiredHash(normalize(desired)) || !created.Enabled {
		t.Fatalf("created = %#v", created)
	}
	identity, err := parseModernIdentity(created.Identity)
	if err != nil || identity != (modernIdentity{Detector: "202", Workflow: "101"}) {
		t.Fatalf("identity=%#v err=%v", identity, err)
	}
	if remote.workflowPosts != 1 || remote.detectorPosts != 1 {
		t.Fatalf("create calls workflow=%d detector=%d", remote.workflowPosts, remote.detectorPosts)
	}
	if got := remote.lastWorkflow.ActionFilters[0].Actions; len(got) != 2 || got[0].Type != "slack" || got[0].IntegrationID != 12 || got[0].Config["targetIdentifier"] != "C123" || got[1].Type != "pagerduty" {
		t.Fatalf("actions = %#v", got)
	}
	if got := remote.lastDetector; got.DataSources[0].Aggregate != "count()" || got.DataSources[0].TimeWindow != 300 || numericValue(got.ConditionGroup.Conditions[0].Comparison) != 200 || !reflect.DeepEqual(got.WorkflowIDs, []string{"101"}) {
		t.Fatalf("detector payload = %#v", got)
	}

	// Ensure is marker-idempotent and recovers the existing pair without another POST.
	recovered, err := adapter.Ensure(context.Background(), ModernMetricIssueAdapter, "company", "checkout", "errors", desired, marker)
	if err != nil || recovered.Identity != created.Identity || remote.workflowPosts != 1 || remote.detectorPosts != 1 {
		t.Fatalf("recovered=%#v err=%v posts=%d/%d", recovered, err, remote.workflowPosts, remote.detectorPosts)
	}

	updatedDesired := desired
	updatedDesired.Threshold = 250
	updated, err := adapter.Update(context.Background(), ModernMetricIssueAdapter, "company", "checkout", created.Identity, updatedDesired)
	if err != nil || updated.Identity != created.Identity || updated.DesiredHash != desiredHash(normalize(updatedDesired)) {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if remote.workflowPuts != 1 || remote.detectorPuts != 1 || numericValue(remote.lastDetector.ConditionGroup.Conditions[0].Comparison) != 250 {
		t.Fatalf("update calls=%d/%d payload=%#v", remote.workflowPuts, remote.detectorPuts, remote.lastDetector)
	}

	disabled, err := adapter.SetEnabled(context.Background(), ModernMetricIssueAdapter, "company", "checkout", created.Identity, false)
	if err != nil || disabled.Enabled || remote.workflowToggles != 1 || remote.detectorToggles != 1 {
		t.Fatalf("disabled=%#v err=%v toggles=%d/%d", disabled, err, remote.workflowToggles, remote.detectorToggles)
	}
	foreignMarker := binding.Marker{Origin: "managed", Owner: "other", ManagedBy: "sko"}
	if err := adapter.Delete(context.Background(), ModernMetricIssueAdapter, "company", "checkout", created.Identity, foreignMarker); !errors.Is(err, ErrModernInvalidState) {
		t.Fatalf("foreign marker delete error = %v", err)
	}
	if remote.detectorDeletes != 0 || remote.workflowDeletes != 0 {
		t.Fatal("foreign marker caused a delete")
	}
	if err := adapter.Delete(context.Background(), ModernMetricIssueAdapter, "company", "checkout", created.Identity, marker); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, found, err := adapter.Observe(context.Background(), ModernMetricIssueAdapter, "company", "checkout", created.Identity)
	if err != nil || found || remote.detectorDeletes != 1 || remote.workflowDeletes != 1 {
		t.Fatalf("post-delete found=%v err=%v deletes=%d/%d", found, err, remote.detectorDeletes, remote.workflowDeletes)
	}
}

func TestModernHTTPAdapterRendersUserCountAggregate(t *testing.T) {
	t.Parallel()
	adapter := &ModernHTTPAdapter{Client: &sentryapi.Client{}, Destinations: map[string]binding.DestinationBinding{
		"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
	}}
	desired := Desired{Trigger: TriggerUserCount, WindowMinutes: 10, Threshold: 50, Enabled: true, Notify: []string{"page"}}
	if err := adapter.validate(ModernMetricIssueAdapter, "checkout", "affected-users", desired); err != nil {
		t.Fatalf("user-count desired state rejected: %v", err)
	}
	payload := detectorPayload("affected-users", "101", desired)
	if payload.DataSources[0].Aggregate != "count_unique(tags[sentry:user])" {
		t.Fatalf("detector payload=%#v", payload)
	}
}

func TestModernPayloadRendersTypedIssueAndDynamicMetric(t *testing.T) {
	t.Parallel()
	adapter := &ModernHTTPAdapter{Client: &sentryapi.Client{}, Destinations: map[string]binding.DestinationBinding{
		"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
	}}
	issue := Desired{
		Issue:   &IssueDesired{TriggerMatch: "any", FilterMatch: "all", Triggers: []IssueTriggerDesired{{Type: TriggerFirstSeen}, {Type: TriggerResolved}}, Filters: []IssueFilterDesired{{Type: "priority", Operator: "gte", Value: "high"}}},
		Actions: []ActionDesired{{Destination: "page", PagerDutySeverity: "critical"}}, Enabled: true, ThrottleMinutes: 10,
	}
	workflow, err := adapter.workflowPayload(context.Background(), "company", "issue", issue, []string{"4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(workflow.Triggers.Conditions) != 2 || workflow.Config["frequency"] != int32(10) || len(workflow.ActionFilters) != 1 || workflow.ActionFilters[0].Actions[0].Data["priority"] != "critical" || !reflect.DeepEqual(workflow.DetectorIDs, []string{"4"}) {
		t.Fatalf("workflow=%#v", workflow)
	}
	metric := Desired{Metric: &MetricDesired{Preset: "logs", IntervalMinutes: 5, Detection: DetectionDesired{Kind: "dynamic", Responsiveness: "high", AnomalyDirection: "both"}}, Enabled: true}
	detector := detectorPayload("logs", "101", metric)
	if detector.DataSources[0].QueryType != 1 || detector.DataSources[0].Dataset != "events_analytics_platform" || detector.DataSources[0].Aggregate != "count(message)" || len(detector.ConditionGroup.Conditions) != 1 || detector.ConditionGroup.Conditions[0].Type != "anomaly_detection" || detector.Config["detectionType"] != "dynamic" {
		t.Fatalf("detector=%#v", detector)
	}
}

func TestModernPayloadEncodesPercentChangeAsSentryRatios(t *testing.T) {
	t.Parallel()
	medium := float64(25)
	resolve := float64(5)

	above := detectorPayload("growth", "101", Desired{Metric: &MetricDesired{
		Preset: "logs", IntervalMinutes: 5,
		Detection: DetectionDesired{Kind: "change", Direction: "above", High: 50, Medium: &medium, Resolve: &resolve, CompareMinutes: 60},
	}, Enabled: true})
	conditions := above.ConditionGroup.Conditions
	if len(conditions) != 3 || conditions[0].Type != "gt" || conditions[0].Comparison != float64(150) || conditions[1].Comparison != float64(125) || conditions[2].Type != "lte" || conditions[2].Comparison != float64(105) {
		t.Fatalf("above conditions=%#v", conditions)
	}

	below := detectorPayload("drop", "102", Desired{Metric: &MetricDesired{
		Preset: "logs", IntervalMinutes: 5,
		Detection: DetectionDesired{Kind: "change", Direction: "below", High: 50, Medium: &medium, Resolve: &resolve, CompareMinutes: 60},
	}, Enabled: true})
	conditions = below.ConditionGroup.Conditions
	if len(conditions) != 3 || conditions[0].Type != "lt" || conditions[0].Comparison != float64(50) || conditions[1].Comparison != float64(75) || conditions[2].Type != "gte" || conditions[2].Comparison != float64(95) {
		t.Fatalf("below conditions=%#v", conditions)
	}
}

func TestModernHTTPAdapterConvergesIgnoredDisabledDetectorCreate(t *testing.T) {
	t.Parallel()

	remote := &modernTestRemote{forceDetectorOnCreate: true}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := &ModernHTTPAdapter{Client: modernTestClient(t, server), Destinations: map[string]binding.DestinationBinding{
		"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
	}}
	desired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 20, Enabled: false, Notify: []string{"page"}}
	observed, err := adapter.Ensure(context.Background(), ModernMetricIssueAdapter, "company", "checkout", "disabled", desired, binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"})
	if err != nil || observed.Enabled || remote.workflowToggles != 1 || remote.detectorToggles != 1 {
		t.Fatalf("disabled create=%#v err=%v toggles=%d/%d", observed, err, remote.workflowToggles, remote.detectorToggles)
	}
}

func TestModernHTTPAdapterRecoversLostDetectorCreateResponse(t *testing.T) {
	t.Parallel()

	remote := &modernTestRemote{loseDetectorResponseOnce: true}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := &ModernHTTPAdapter{Client: modernTestClient(t, server), Destinations: map[string]binding.DestinationBinding{
		"chat": {Kind: "slack", State: "Active", IntegrationIdentity: "12", ResourceIdentity: "C123", ResourceName: "incidents"},
	}}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"}
	desired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 200, Enabled: true, Notify: []string{"chat"}}

	recovered, err := adapter.Ensure(context.Background(), ModernMetricIssueAdapter, "company", "checkout", "errors", desired, marker)
	if err != nil || recovered.Identity == "" || remote.workflowPosts != 1 || remote.detectorPosts != 1 {
		t.Fatalf("recovered=%#v err=%v posts=%d/%d", recovered, err, remote.workflowPosts, remote.detectorPosts)
	}
}

func TestModernHTTPAdapterRejectsPartialDetectorAfterAmbiguousCreate(t *testing.T) {
	t.Parallel()

	remote := &modernTestRemote{loseDetectorResponseOnce: true, dropConditionGroup: true}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := &ModernHTTPAdapter{Client: modernTestClient(t, server)}
	desired := Desired{Metric: &MetricDesired{Preset: "error-count", IntervalMinutes: 5, Detection: DetectionDesired{Kind: "dynamic", Responsiveness: "medium", AnomalyDirection: "both"}}, Enabled: true}

	_, err := adapter.Ensure(context.Background(), ModernMetricIssueAdapter, "company", "checkout", "dynamic", desired, binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"})
	if err == nil || remote.detectorPosts != 1 {
		t.Fatalf("partial ambiguous create err=%v detectorPosts=%d", err, remote.detectorPosts)
	}
}

type modernTestRemote struct {
	workflowExists                                                     bool
	detectorExists                                                     bool
	workflowName                                                       string
	detectorName                                                       string
	workflowOn                                                         bool
	detectorOn                                                         bool
	lastWorkflow                                                       modernWorkflowPayload
	lastDetector                                                       modernDetectorPayload
	workflowPosts, detectorPosts, workflowPuts, detectorPuts           int
	workflowToggles, detectorToggles, workflowDeletes, detectorDeletes int
	loseDetectorResponseOnce                                           bool
	forceDetectorOnCreate                                              bool
	dropConditionGroup                                                 bool
}

func (r *modernTestRemote) serveHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := request.URL.Path
	switch {
	case request.Method == http.MethodGet && path == "/api/0/organizations/company/workflows/":
		if !r.workflowExists {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{r.workflowResponse()})
	case request.Method == http.MethodGet && path == "/api/0/organizations/company/detectors/":
		if !r.detectorExists {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{r.detectorResponse()})
	case request.Method == http.MethodGet && path == "/api/0/organizations/company/workflows/101/":
		if !r.workflowExists {
			http.NotFound(w, request)
			return
		}
		writeJSON(w, r.workflowResponse())
	case request.Method == http.MethodGet && path == "/api/0/organizations/company/detectors/202/":
		if !r.detectorExists {
			http.NotFound(w, request)
			return
		}
		writeJSON(w, r.detectorResponse())
	case request.Method == http.MethodPost && path == "/api/0/organizations/company/workflows/":
		r.workflowPosts++
		if json.NewDecoder(request.Body).Decode(&r.lastWorkflow) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.workflowExists, r.workflowName, r.workflowOn = true, r.lastWorkflow.Name, r.lastWorkflow.Enabled
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r.workflowResponse())
	case request.Method == http.MethodPost && path == "/api/0/organizations/company/projects/checkout/detectors/":
		r.detectorPosts++
		if json.NewDecoder(request.Body).Decode(&r.lastDetector) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.detectorExists, r.detectorName, r.detectorOn = true, r.lastDetector.Name, r.lastDetector.Enabled
		if r.forceDetectorOnCreate {
			r.detectorOn = true
		}
		if r.loseDetectorResponseOnce {
			r.loseDetectorResponseOnce = false
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r.detectorResponse())
	case request.Method == http.MethodPut && path == "/api/0/organizations/company/workflows/101/":
		r.workflowPuts++
		_ = json.NewDecoder(request.Body).Decode(&r.lastWorkflow)
		r.workflowName, r.workflowOn = r.lastWorkflow.Name, r.lastWorkflow.Enabled
		writeJSON(w, r.workflowResponse())
	case request.Method == http.MethodPut && path == "/api/0/organizations/company/detectors/202/":
		r.detectorPuts++
		_ = json.NewDecoder(request.Body).Decode(&r.lastDetector)
		r.detectorName, r.detectorOn = r.lastDetector.Name, r.lastDetector.Enabled
		writeJSON(w, r.detectorResponse())
	case request.Method == http.MethodPut && path == "/api/0/organizations/company/workflows/" && request.URL.Query().Get("id") == "101":
		r.workflowToggles++
		var body struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		r.workflowOn = body.Enabled
		writeJSON(w, r.workflowResponse())
	case request.Method == http.MethodPut && path == "/api/0/organizations/company/detectors/" && request.URL.Query().Get("id") == "202":
		r.detectorToggles++
		var body struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		r.detectorOn = body.Enabled
		writeJSON(w, r.detectorResponse())
	case request.Method == http.MethodDelete && path == "/api/0/organizations/company/detectors/202/":
		r.detectorDeletes++
		r.detectorExists = false
		w.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodDelete && path == "/api/0/organizations/company/workflows/101/":
		r.workflowDeletes++
		r.workflowExists = false
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, fmt.Sprintf("unexpected %s %s?%s", request.Method, path, request.URL.RawQuery), http.StatusNotFound)
	}
}

func (r *modernTestRemote) workflowResponse() modernWorkflowResponse {
	detectors := []string{}
	if r.detectorExists {
		detectors = []string{"202"}
	}
	response := modernWorkflowResponse{}
	payload, _ := json.Marshal(r.lastWorkflow)
	_ = json.Unmarshal(payload, &response)
	response.ID, response.Name, response.Enabled, response.DetectorIDs = "101", r.workflowName, r.workflowOn, detectors
	return response
}

func (r *modernTestRemote) detectorResponse() modernDetectorResponse {
	workflows := []string{}
	if r.workflowExists {
		workflows = []string{"101"}
	}
	conditionGroup := r.lastDetector.ConditionGroup
	dataSources := make([]modernDataSourceResponse, len(r.lastDetector.DataSources))
	for index, source := range r.lastDetector.DataSources {
		dataSources[index].QueryObj.SnubaQuery.Dataset = source.Dataset
		dataSources[index].QueryObj.SnubaQuery.Query = source.Query
		dataSources[index].QueryObj.SnubaQuery.Aggregate = source.Aggregate
		dataSources[index].QueryObj.SnubaQuery.TimeWindow = source.TimeWindow
		dataSources[index].QueryObj.SnubaQuery.Environment = source.Environment
		dataSources[index].QueryObj.SnubaQuery.EventTypes = source.EventTypes
	}
	var owner *modernDetectorOwnerResponse
	if r.lastDetector.Owner != nil {
		parts := strings.SplitN(*r.lastDetector.Owner, ":", 2)
		owner = &modernDetectorOwnerResponse{Type: parts[0], ID: parts[1]}
	}
	response := modernDetectorResponse{
		ID: "202", ProjectID: "9", Name: r.detectorName, Type: "metric_issue", Enabled: r.detectorOn,
		Config: r.lastDetector.Config, ConditionGroup: &conditionGroup, DataSources: dataSources,
		WorkflowIDs: workflows, Description: r.lastDetector.Description, Owner: owner,
	}
	if r.dropConditionGroup {
		response.ConditionGroup = nil
	}
	return response
}

func modernTestClient(t *testing.T, server *httptest.Server) *sentryapi.Client {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, Token: "token", CABundle: ca})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }

func numericValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}
