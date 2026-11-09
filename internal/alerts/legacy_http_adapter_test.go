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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestLegacyHTTPAdapterIssueLifecycleAndLostResponseRecovery(t *testing.T) {
	t.Parallel()

	remote := &legacyTestRemote{loseIssueResponseOnce: true}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := legacyTestAdapter(t, server)
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"}
	desired := Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat", "page"}}

	if _, err := adapter.Ensure(context.Background(), LegacyIssueAdapter, "company", "checkout", "new-errors", desired, marker); err == nil {
		t.Fatal("lost create response was reported as success")
	}
	created, err := adapter.Ensure(context.Background(), LegacyIssueAdapter, "company", "checkout", "new-errors", desired, marker)
	if err != nil || created.Identity != "11" || created.Marker != marker || created.DesiredHash != desiredHash(normalize(desired)) || !created.Enabled {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if remote.issuePosts != 1 {
		t.Fatalf("lost response caused %d issue POSTs", remote.issuePosts)
	}
	if got := remote.lastIssue; got.Conditions[0]["id"] != "sentry.rules.conditions.first_seen_event.FirstSeenEventCondition" || len(got.Actions) != 2 || got.Actions[0].Workspace != "12" || got.Actions[0].ChannelID != "C123" || got.Actions[1].Service != "567" {
		t.Fatalf("issue payload=%#v", got)
	}

	updatedDesired := desired
	updatedDesired.Trigger = TriggerRegression
	updated, err := adapter.Update(context.Background(), LegacyIssueAdapter, "company", "checkout", created.Identity, updatedDesired)
	if err != nil || updated.Identity != created.Identity || updated.DesiredHash != desiredHash(normalize(updatedDesired)) || remote.issuePuts != 1 {
		t.Fatalf("updated=%#v err=%v puts=%d", updated, err, remote.issuePuts)
	}
	if remote.lastIssue.Conditions[0]["id"] != "sentry.rules.conditions.regression_event.RegressionEventCondition" {
		t.Fatalf("updated issue payload=%#v", remote.lastIssue)
	}
	if _, err := adapter.SetEnabled(context.Background(), LegacyIssueAdapter, "company", "checkout", created.Identity, false); !errors.Is(err, ErrLegacyToggleUnsupported) {
		t.Fatalf("disable error=%v", err)
	}

	foreign := binding.Marker{Origin: "managed", Owner: "other", ManagedBy: "sko"}
	if err := adapter.Delete(context.Background(), LegacyIssueAdapter, "company", "checkout", created.Identity, foreign); !errors.Is(err, ErrLegacyInvalidState) || remote.issueDeletes != 0 {
		t.Fatalf("foreign delete err=%v deletes=%d", err, remote.issueDeletes)
	}
	if err := adapter.Delete(context.Background(), LegacyIssueAdapter, "company", "checkout", created.Identity, marker); err != nil {
		t.Fatal(err)
	}
	_, found, err := adapter.Observe(context.Background(), LegacyIssueAdapter, "company", "checkout", created.Identity)
	if err != nil || found || remote.issueDeletes != 1 {
		t.Fatalf("post-delete found=%v err=%v deletes=%d", found, err, remote.issueDeletes)
	}
}

func TestLegacyHTTPAdapterMetricLifecycleAndPayload(t *testing.T) {
	t.Parallel()

	remote := &legacyTestRemote{}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	adapter := legacyTestAdapter(t, server)
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"}
	desired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 200, Enabled: true, Notify: []string{"chat", "page"}}

	created, err := adapter.Ensure(context.Background(), LegacyMetricAdapter, "company", "checkout", "error-volume", desired, marker)
	if err != nil || created.Identity != "22" || remote.metricPosts != 1 {
		t.Fatalf("created=%#v err=%v posts=%d", created, err, remote.metricPosts)
	}
	if got := remote.lastMetric; got.QueryType != 0 || got.Dataset != "events" || got.Aggregate != "count()" || got.TimeWindow != 5 || got.Triggers[0].AlertThreshold != 200 || len(got.Triggers[0].Actions) != 2 {
		t.Fatalf("metric payload=%#v", got)
	}
	if got := remote.lastMetric.Triggers[0].Actions; got[0].Type != "slack" || got[0].IntegrationID != 12 || got[0].TargetIdentifier != "incidents" || got[0].InputChannelID != "C123" || got[1].Type != "pagerduty" || got[1].TargetIdentifier != float64(567) {
		t.Fatalf("metric actions=%#v", got)
	}
	second, err := adapter.Ensure(context.Background(), LegacyMetricAdapter, "company", "checkout", "error-volume", desired, marker)
	if err != nil || second.Identity != created.Identity || remote.metricPosts != 1 {
		t.Fatalf("idempotent ensure=%#v err=%v posts=%d", second, err, remote.metricPosts)
	}

	updatedDesired := desired
	updatedDesired.Threshold = 250
	updated, err := adapter.Update(context.Background(), LegacyMetricAdapter, "company", "checkout", created.Identity, updatedDesired)
	if err != nil || updated.DesiredHash != desiredHash(normalize(updatedDesired)) || remote.metricPuts != 1 || remote.lastMetric.Triggers[0].AlertThreshold != 250 {
		t.Fatalf("updated=%#v err=%v payload=%#v", updated, err, remote.lastMetric)
	}
	if _, err := adapter.SetEnabled(context.Background(), LegacyMetricAdapter, "company", "checkout", created.Identity, true); !errors.Is(err, ErrLegacyToggleUnsupported) {
		t.Fatalf("metric enable error=%v", err)
	}
	if err := adapter.Delete(context.Background(), LegacyMetricAdapter, "company", "checkout", created.Identity, marker); err != nil {
		t.Fatal(err)
	}
	_, found, err := adapter.Observe(context.Background(), LegacyMetricAdapter, "company", "checkout", created.Identity)
	if err != nil || found || remote.metricDeletes != 1 {
		t.Fatalf("post-delete found=%v err=%v deletes=%d", found, err, remote.metricDeletes)
	}
}

func TestLegacyHTTPAdapterAcceptsDecimalMetricThresholds(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/0/organizations/company/alert-rules/" || request.Method != http.MethodGet {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"22","name":"checkout:volume:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef [managed;o=payments;m=sko]","status":0,"triggers":[{"label":"critical","alertThreshold":20.0,"actions":[]}]}]`))
	}))
	defer server.Close()

	adapter := &LegacyHTTPAdapter{Client: modernTestClient(t, server)}
	items, err := adapter.list(context.Background(), LegacyMetricAdapter, "company", "checkout")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	response, ok := items[0].(legacyMetricResponse)
	if !ok || len(response.Triggers) != 1 || response.Triggers[0].AlertThreshold != 20 {
		t.Fatalf("response=%#v", items[0])
	}
}

func TestLegacyHTTPAdapterRendersUserCountAggregate(t *testing.T) {
	t.Parallel()
	adapter := &LegacyHTTPAdapter{Client: &sentryapi.Client{}, Destinations: map[string]binding.DestinationBinding{
		"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
	}}
	desired := Desired{Trigger: TriggerUserCount, WindowMinutes: 10, Threshold: 50, Enabled: true, Notify: []string{"page"}}
	if err := adapter.validate(LegacyMetricAdapter, "checkout", "affected-users", desired); err != nil {
		t.Fatalf("user-count desired state rejected: %v", err)
	}
	payload, err := adapter.payload(LegacyMetricAdapter, "checkout", "affected-users", desired)
	if err != nil || payload.(legacyMetricPayload).Aggregate != "count_unique(user)" {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}
}

func TestLegacyRouterSupportsMixedIssueAndMetricIdentities(t *testing.T) {
	t.Parallel()
	remote := &legacyTestRemote{}
	server := httptest.NewTLSServer(http.HandlerFunc(remote.serveHTTP))
	defer server.Close()
	router := &LegacyRouterAdapter{Legacy: legacyTestAdapter(t, server)}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sko"}
	issue, err := router.Ensure(context.Background(), LegacyCombinedAdapter, "company", "checkout", "new-errors", Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}, marker)
	if err != nil {
		t.Fatal(err)
	}
	metric, err := router.Ensure(context.Background(), LegacyCombinedAdapter, "company", "checkout", "volume", Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 10, Enabled: true, Notify: []string{"page"}}, marker)
	if err != nil {
		t.Fatal(err)
	}
	issueIdentity, issueChild, err := parseLegacyRoutedIdentity(issue.Adapter, issue.Identity)
	if err != nil || issueChild != LegacyIssueAdapter || issueIdentity.ID != "11" {
		t.Fatalf("issue identity=%#v child=%q err=%v", issueIdentity, issueChild, err)
	}
	metricIdentity, metricChild, err := parseLegacyRoutedIdentity(metric.Adapter, metric.Identity)
	if err != nil || metricChild != LegacyMetricAdapter || metricIdentity.ID != "22" {
		t.Fatalf("metric identity=%#v child=%q err=%v", metricIdentity, metricChild, err)
	}
	observed, found, err := router.Observe(context.Background(), LegacyCombinedAdapter, "company", "checkout", metric.Identity)
	if err != nil || !found || observed.Identity != metric.Identity || observed.Adapter != LegacyCombinedAdapter {
		t.Fatalf("observed=%#v found=%v err=%v", observed, found, err)
	}
	changedFamily := Desired{Trigger: TriggerRegression, Enabled: true, Notify: []string{"chat"}}
	if _, err := router.Update(context.Background(), LegacyCombinedAdapter, "company", "checkout", metric.Identity, changedFamily); !errors.Is(err, ErrUnsupportedUpdate) {
		t.Fatalf("family-changing update error=%v", err)
	}
	if _, _, err := parseLegacyRoutedIdentity(LegacyCombinedAdapter, `{"kind":"issue","id":"11","extra":true}`); err == nil {
		t.Fatal("identity with unknown field was accepted")
	}
}

func legacyTestAdapter(t *testing.T, server *httptest.Server) *LegacyHTTPAdapter {
	t.Helper()
	return &LegacyHTTPAdapter{
		Client: modernTestClient(t, server),
		Destinations: map[string]binding.DestinationBinding{
			"chat": {Kind: "slack", State: "Active", IntegrationIdentity: "12", ResourceIdentity: "C123", ResourceName: "incidents"},
			"page": {Kind: "pagerduty", State: "Active", IntegrationIdentity: "34", ResourceIdentity: "567", ResourceName: "checkout"},
		},
	}
}

type legacyTestRemote struct {
	issueExists, issueDisabled, metricExists bool
	issueName, metricName                    string
	lastIssue                                legacyIssuePayload
	lastMetric                               legacyMetricPayload
	issuePosts, issuePuts, issueDeletes      int
	metricPosts, metricPuts, metricDeletes   int
	loseIssueResponseOnce                    bool
}

func (r *legacyTestRemote) serveHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case request.URL.Path == "/api/0/projects/company/checkout/rules/" && request.Method == http.MethodGet:
		if !r.issueExists || r.issueDisabled {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{r.issueResponse()})
	case request.URL.Path == "/api/0/projects/company/checkout/rules/" && request.Method == http.MethodPost:
		r.issuePosts++
		if json.NewDecoder(request.Body).Decode(&r.lastIssue) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.issueExists, r.issueDisabled, r.issueName = true, false, r.lastIssue.Name
		if r.loseIssueResponseOnce {
			r.loseIssueResponseOnce = false
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
				return
			}
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r.issueResponse())
	case request.URL.Path == "/api/0/projects/company/checkout/rules/11/" && request.Method == http.MethodGet:
		if !r.issueExists {
			http.NotFound(w, request)
			return
		}
		writeJSON(w, r.issueResponse())
	case request.URL.Path == "/api/0/projects/company/checkout/rules/11/" && request.Method == http.MethodPut:
		r.issuePuts++
		_ = json.NewDecoder(request.Body).Decode(&r.lastIssue)
		r.issueName, r.issueDisabled = r.lastIssue.Name, false
		writeJSON(w, r.issueResponse())
	case request.URL.Path == "/api/0/projects/company/checkout/rules/11/" && request.Method == http.MethodDelete:
		r.issueDeletes++
		r.issueDisabled = true
		w.WriteHeader(http.StatusNoContent)
	case request.URL.Path == "/api/0/organizations/company/alert-rules/" && request.Method == http.MethodGet:
		if !r.metricExists {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{r.metricResponse()})
	case request.URL.Path == "/api/0/organizations/company/alert-rules/" && request.Method == http.MethodPost:
		r.metricPosts++
		if json.NewDecoder(request.Body).Decode(&r.lastMetric) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.metricExists, r.metricName = true, r.lastMetric.Name
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, r.metricResponse())
	case request.URL.Path == "/api/0/organizations/company/alert-rules/22/" && request.Method == http.MethodGet:
		if !r.metricExists {
			http.NotFound(w, request)
			return
		}
		writeJSON(w, r.metricResponse())
	case request.URL.Path == "/api/0/organizations/company/alert-rules/22/" && request.Method == http.MethodPut:
		r.metricPuts++
		_ = json.NewDecoder(request.Body).Decode(&r.lastMetric)
		r.metricName = r.lastMetric.Name
		writeJSON(w, r.metricResponse())
	case request.URL.Path == "/api/0/organizations/company/alert-rules/22/" && request.Method == http.MethodDelete:
		r.metricDeletes++
		r.metricExists = false
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, request)
	}
}

func (r *legacyTestRemote) issueResponse() legacyIssueResponse {
	status := "active"
	if r.issueDisabled {
		status = "disabled"
	}
	return legacyIssueResponse{ID: "11", Name: r.issueName, Status: status}
}

func (r *legacyTestRemote) metricResponse() legacyMetricResponse {
	return legacyMetricResponse{ID: "22", Name: r.metricName, Triggers: r.lastMetric.Triggers}
}
