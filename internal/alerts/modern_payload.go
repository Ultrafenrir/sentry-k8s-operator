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
	"fmt"
	"net/url"
	"path"
	"strconv"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func modernMetricSource(desired Desired) (string, string, string, []string, error) {
	preset := "error-count"
	if desired.Metric != nil {
		preset = desired.Metric.Preset
	}
	if desired.Metric == nil && desired.Trigger == TriggerUserCount {
		preset = "affected-users"
	}
	switch preset {
	case "affected-users":
		return "events", "count_unique(tags[sentry:user])", "is:unresolved", []string{"error", "default"}, nil
	case "crash-free-sessions":
		return "metrics", "percentage(sessions_crashed, sessions) AS _crash_rate_alert_aggregate", "", []string{}, nil
	case "crash-free-users":
		return "metrics", "percentage(users_crashed, users) AS _crash_rate_alert_aggregate", "", []string{}, nil
	case "throughput":
		return "events_analytics_platform", "count(span.duration)", "", []string{"trace_item_span"}, nil
	case "duration":
		return "events_analytics_platform", "p95(span.duration)", "", []string{"trace_item_span"}, nil
	case "failure-rate":
		return "events_analytics_platform", "failure_rate()", "", []string{"trace_item_span"}, nil
	case "largest-contentful-paint":
		return "events_analytics_platform", "p75(measurements.lcp)", "is_transaction:true", []string{"trace_item_span"}, nil
	case "logs":
		return "events_analytics_platform", "count(message)", "", []string{"trace_item_log"}, nil
	case "application-metric":
		if desired.Metric == nil || desired.Metric.ApplicationMetric == nil {
			return "", "", "", nil, ErrInvalidInput
		}
		aggregate, err := applicationMetricAggregate(*desired.Metric.ApplicationMetric)
		return "events_analytics_platform", aggregate, "", []string{"trace_item_metric"}, err
	default:
		return "events", "count()", "is:unresolved", []string{"error", "default"}, nil
	}
}

func applicationMetricAggregate(expression ApplicationMetricExpressionDesired) (string, error) {
	left, err := applicationMetricTermAggregate(expression.Left)
	if err != nil {
		return "", err
	}
	if expression.Operator == "" && expression.Right == nil {
		return left, nil
	}
	if expression.Right == nil {
		return "", ErrInvalidInput
	}
	right, err := applicationMetricTermAggregate(*expression.Right)
	if err != nil {
		return "", err
	}
	operator, ok := map[string]string{"add": "+", "subtract": "-", "multiply": "*", "divide": "/"}[expression.Operator]
	if !ok {
		return "", ErrInvalidInput
	}
	return "equation|" + left + " " + operator + " " + right, nil
}

func applicationMetricTermAggregate(term ApplicationMetricTermDesired) (string, error) {
	if !validApplicationMetricTerm(term) {
		return "", ErrInvalidInput
	}
	aggregation := map[string]string{"per-second": "per_second", "per-minute": "per_minute"}[term.Aggregation]
	if aggregation == "" {
		aggregation = term.Aggregation
	}
	unit := term.Unit
	if unit == "" {
		unit = "none"
	}
	return fmt.Sprintf("%s(value,%s,%s,%s)", aggregation, term.Name, term.Type, unit), nil
}

func modernDetection(desired Desired) ([]modernCondition, map[string]any) {
	detection := DetectionDesired{Kind: "static", Direction: "above", High: float64(desired.Threshold)}
	if desired.Metric != nil {
		detection = desired.Metric.Detection
	}
	config := map[string]any{"detectionType": detection.Kind}
	if detection.Kind == "dynamic" {
		thresholdType := map[string]int{"above": 0, "below": 1, "both": 2}[detection.AnomalyDirection]
		return []modernCondition{{Type: "anomaly_detection", Comparison: map[string]any{
			"seasonality": "auto", "sensitivity": detection.Responsiveness, "thresholdType": thresholdType,
		}, ConditionResult: 75}}, config
	}
	conditionType := "gt"
	resolveType := "lte"
	if detection.Direction == "below" {
		conditionType, resolveType = "lt", "gte"
	}
	if detection.Kind == "change" {
		config["detectionType"] = "percent"
		config["comparisonDelta"] = detection.CompareMinutes * 60
		detection.High = modernPercentComparison(detection.Direction, detection.High)
		if detection.Medium != nil {
			value := modernPercentComparison(detection.Direction, *detection.Medium)
			detection.Medium = &value
		}
		if detection.Resolve != nil {
			value := modernPercentComparison(detection.Direction, *detection.Resolve)
			detection.Resolve = &value
		}
	}
	conditions := []modernCondition{{Type: conditionType, Comparison: detection.High, ConditionResult: 75}}
	if detection.Medium != nil {
		conditions = append(conditions, modernCondition{Type: conditionType, Comparison: *detection.Medium, ConditionResult: 50})
	}
	resolve := float64(0)
	if detection.Resolve != nil {
		resolve = *detection.Resolve
	} else if detection.Medium != nil {
		resolve = *detection.Medium
	}
	conditions = append(conditions, modernCondition{Type: resolveType, Comparison: resolve, ConditionResult: 0})
	return conditions, config
}

// Sentry's modern detector API stores percent-change thresholds as a ratio
// relative to 100. The UI value "50% higher" is therefore 150, while "50%
// lower" is 50. Keep that wire representation inside the adapter.
func modernPercentComparison(direction string, percent float64) float64 {
	if direction == "below" {
		return 100 - percent
	}
	return 100 + percent
}

func modernWorkflowTriggers(desired Desired) (modernConditionGroup, error) {
	if desired.Issue == nil {
		return modernConditionGroup{LogicType: "any", Conditions: []modernCondition{}}, nil
	}
	conditions := make([]modernCondition, 0, len(desired.Issue.Triggers))
	for _, trigger := range desired.Issue.Triggers {
		triggerType := map[Trigger]string{
			TriggerFirstSeen:  "first_seen_event",
			TriggerRegression: "regression_event",
			TriggerResolved:   "issue_resolved_trigger",
			TriggerEscalation: "new_high_priority_issue",
			TriggerEventCount: "event_frequency_count",
			TriggerUserCount:  "event_unique_user_frequency_count",
			TriggerSessions:   "percent_sessions_count",
		}[trigger.Type]
		if triggerType == "" {
			return modernConditionGroup{}, ErrModernInvalidState
		}
		comparison := any(nil)
		if trigger.WindowMinutes != 0 {
			comparison = map[string]any{"interval": legacyInterval(trigger.WindowMinutes), "value": trigger.Threshold}
		}
		if comparison == nil {
			comparison = true
		}
		conditions = append(conditions, modernCondition{Type: triggerType, Comparison: comparison, ConditionResult: true})
	}
	logic := desired.Issue.TriggerMatch
	if logic == "" || logic == "any" {
		logic = "any-short"
	}
	return modernConditionGroup{LogicType: logic, Conditions: conditions}, nil
}

func (a *ModernHTTPAdapter) actionFilters(ctx context.Context, organization string, desired Desired) ([]modernActionFilter, error) {
	actions := desired.Actions
	if len(actions) == 0 {
		actions = make([]ActionDesired, 0, len(desired.Notify))
		for _, name := range desired.Notify {
			actions = append(actions, ActionDesired{Destination: name, Status: "triggered"})
		}
	}
	grouped := make(map[string][]modernAction)
	order := make([]string, 0, 4)
	for _, desiredAction := range actions {
		status := desiredAction.Status
		if status == "" {
			status = "triggered"
		}
		if _, exists := grouped[status]; !exists {
			order = append(order, status)
		}
		var action modernAction
		var err error
		switch {
		case desiredAction.Destination != "":
			action, err = a.destinationAction(desiredAction.Destination, desiredAction.PagerDutySeverity)
		case desiredAction.EmailTarget != "":
			action, err = a.emailAction(ctx, organization, desiredAction)
		case desiredAction.GitHubIssue != nil:
			action, err = a.githubIssueAction(ctx, organization, *desiredAction.GitHubIssue)
		default:
			err = ErrModernDestinationUnsupported
		}
		if err != nil {
			return nil, err
		}
		grouped[status] = append(grouped[status], action)
	}
	result := make([]modernActionFilter, 0, len(order))
	for _, status := range order {
		conditions, err := modernIssueFilters(desired)
		if err != nil {
			return nil, err
		}
		if status != "triggered" {
			priority, ok := map[string]int{"resolved": 0, "medium": 50, "high": 75}[status]
			if !ok {
				return nil, ErrModernInvalidState
			}
			conditionType := "issue_priority_greater_or_equal"
			if status == "medium" {
				conditionType = "issue_priority_equals"
			} else if status == "resolved" {
				conditionType = "issue_resolution_change"
			}
			conditions = append(conditions, modernCondition{Type: conditionType, Comparison: priority, ConditionResult: true})
		}
		logic := "all"
		if desired.Issue != nil && desired.Issue.FilterMatch != "" {
			logic = desired.Issue.FilterMatch
		}
		result = append(result, modernActionFilter{LogicType: logic, Conditions: conditions, Actions: grouped[status]})
	}
	return result, nil
}

func (a *ModernHTTPAdapter) githubIssueAction(ctx context.Context, organization string, desired GitHubIssueActionDesired) (modernAction, error) {
	integrationID, err := a.resolveIntegrationID(ctx, organization, desired.Provider, desired.IntegrationName)
	if err != nil {
		return modernAction{}, err
	}
	additionalFields := map[string]any{
		"repo": desired.RepositoryName, "labels": append([]string(nil), desired.Labels...), "assignee": desired.Assignee,
	}
	return modernAction{
		Type: desired.Provider, IntegrationID: integrationID,
		Data:   map[string]any{"additional_fields": additionalFields, "dynamic_form_fields": githubIssueFormFields(organization, integrationID, desired)},
		Config: map[string]any{"targetType": "specific", "targetIdentifier": nil, "targetDisplay": nil},
	}, nil
}

func githubIssueFormFields(organization string, integrationID int64, desired GitHubIssueActionDesired) []any {
	repositoryLabel := path.Base(desired.RepositoryName)
	searchURL := fmt.Sprintf("/extensions/github/search/%s/%d/", url.PathEscape(organization), integrationID)
	fields := []any{map[string]any{
		"name": "repo", "label": "GitHub Repository", "type": "select", "default": desired.RepositoryName,
		"choices": [][]string{{desired.RepositoryName, repositoryLabel}}, "url": searchURL, "updatesForm": true, "required": true,
		"prefetch": true,
	}}
	assigneeChoices := [][]string{}
	if desired.Assignee != "" {
		assigneeChoices = append(assigneeChoices, []string{desired.Assignee, desired.Assignee})
	}
	fields = append(fields, map[string]any{
		"name": "assignee", "label": "Assignee", "type": "select", "default": desired.Assignee,
		"choices": assigneeChoices, "url": searchURL, "required": false, "prefetch": true,
		"dependsOn": []string{"repo"},
	})
	labelChoices := make([][]string, 0, len(desired.Labels))
	for _, label := range desired.Labels {
		labelChoices = append(labelChoices, []string{label, label})
	}
	return append(fields, map[string]any{
		"name": "labels", "label": "Labels", "type": "select", "default": append([]string(nil), desired.Labels...),
		"choices": labelChoices, "url": searchURL, "multiple": true, "required": false, "prefetch": true,
		"dependsOn": []string{"repo"},
	})
}

func modernIssueFilters(desired Desired) ([]modernCondition, error) {
	if desired.Issue == nil {
		return []modernCondition{}, nil
	}
	result := make([]modernCondition, 0, len(desired.Issue.Filters))
	for _, filter := range desired.Issue.Filters {
		condition, err := modernIssueFilter(filter)
		if err != nil {
			return nil, err
		}
		result = append(result, condition)
	}
	return result, nil
}

func modernIssueFilter(filter IssueFilterDesired) (modernCondition, error) {
	match := map[string]string{"eq": "eq", "neq": "ne", "contains": "co", "gte": "gte", "lte": "lte", "gt": "gt", "lt": "lt"}[filter.Operator]
	condition := modernCondition{ConditionResult: true}
	switch filter.Type {
	case "age":
		value, err := strconv.Atoi(filter.Value)
		if err != nil || value < 1 || match != "gt" && match != "gte" && match != "lt" && match != "lte" {
			return modernCondition{}, ErrModernInvalidState
		}
		comparisonType := "older"
		if match == "lt" || match == "lte" {
			comparisonType = "newer"
		}
		condition.Type, condition.Comparison = "age_comparison", map[string]any{"time": "minute", "value": value, "comparisonType": comparisonType}
	case "assignment":
		if filter.Value != "unassigned" {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "assigned_to", map[string]any{"targetType": "Unassigned", "targetIdentifier": ""}
	case "frequency":
		value, err := strconv.Atoi(filter.Value)
		if err != nil || value < 1 {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "issue_occurrences", map[string]any{"value": value}
	case "priority":
		priority, ok := map[string]int{"high": 75, "medium": 50, "low": 25}[filter.Value]
		if !ok {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "issue_priority_greater_or_equal", priority
	case "category":
		category, ok := map[string]int{"error": 1, "feedback": 6, "outage": 10, "metric": 11, "database": 12, "http": 13, "frontend": 14, "mobile": 15, "preprod": 17, "configuration": 19}[filter.Value]
		if !ok {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "issue_category", map[string]any{"value": category}
	case "event-attribute":
		if match == "" || filter.Key == "" {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "event_attribute", map[string]any{"attribute": filter.Key, "match": match, "value": filter.Value}
	case "tag":
		if match == "" || filter.Key == "" {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "tagged_event", map[string]any{"key": filter.Key, "match": match, "value": filter.Value}
	case "level":
		level, ok := map[string]int{"fatal": 50, "error": 40, "warning": 30, "info": 20, "debug": 10, "sample": 0}[filter.Value]
		if !ok || match != "eq" && match != "gte" && match != "lte" {
			return modernCondition{}, ErrModernInvalidState
		}
		condition.Type, condition.Comparison = "level", map[string]any{"level": level, "match": match}
	case "latest-release":
		condition.Type, condition.Comparison = "latest_release", true
	case "release-age":
		if filter.Key == "" || filter.Value != "oldest" && filter.Value != "newest" || match != "gt" && match != "gte" && match != "lt" && match != "lte" {
			return modernCondition{}, ErrModernInvalidState
		}
		ageComparison := "older"
		if match == "lt" || match == "lte" {
			ageComparison = "newer"
		}
		condition.Type, condition.Comparison = "latest_adopted_release", map[string]any{"environment": filter.Key, "ageComparison": ageComparison, "releaseAgeType": filter.Value}
	case "type":
		condition.Type, condition.Comparison = "issue_type", map[string]any{"value": filter.Value}
	default:
		return modernCondition{}, ErrModernInvalidState
	}
	return condition, nil
}

func (a *ModernHTTPAdapter) emailAction(ctx context.Context, organization string, desired ActionDesired) (modernAction, error) {
	config := map[string]any{}
	data := map[string]any{}
	switch desired.EmailTarget {
	case "suggested-assignees":
		config["targetType"] = "issue_owners"
		config["targetIdentifier"] = ""
		data["fallthroughType"] = "ActiveMembers"
	case "team":
		teamID, err := a.resolveTeamID(ctx, organization, desired.EmailTeam)
		if err != nil {
			return modernAction{}, err
		}
		config["targetType"] = "team"
		config["targetIdentifier"] = teamID
	default:
		return modernAction{}, ErrModernDestinationUnsupported
	}
	return modernAction{Type: "email", Data: data, Config: config}, nil
}

func (a *ModernHTTPAdapter) resolveTeamID(ctx context.Context, organization, slug string) (string, error) {
	type team struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	items, err := sentryapi.CollectPages[team](ctx, a.Client, organizationPath(organization)+"teams/?query="+url.QueryEscape(slug), "list-modern-teams")
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Slug == slug && item.ID != "" {
			return item.ID, nil
		}
	}
	return "", fmt.Errorf("%w: team %q", ErrModernDestinationUnsupported, slug)
}

func (a *ModernHTTPAdapter) resolveIntegrationID(ctx context.Context, organization, provider, name string) (int64, error) {
	type integration struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	path := organizationPath(organization) + "integrations/?providerKey=" + url.QueryEscape(provider) + "&includeConfig=0"
	items, err := sentryapi.CollectPages[integration](ctx, a.Client, path, "list-modern-ticket-integrations")
	if err != nil {
		return 0, err
	}
	identity := int64(0)
	for _, item := range items {
		if item.Name != name {
			continue
		}
		parsed, err := strconv.ParseInt(item.ID, 10, 64)
		if err != nil || parsed <= 0 || identity != 0 {
			return 0, ErrModernDestinationUnsupported
		}
		identity = parsed
	}
	if identity == 0 {
		return 0, fmt.Errorf("%w: %s integration %q", ErrModernDestinationUnsupported, provider, name)
	}
	return identity, nil
}
