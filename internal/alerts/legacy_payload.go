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
	"net/url"
	"strconv"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func (a *LegacyHTTPAdapter) typedLegacyPayload(ctx context.Context, adapter, organization, projectSlug, name string, desired Desired) (any, error) {
	if adapter == LegacyIssueAdapter && desired.Issue != nil {
		conditions, err := legacyIssueConditions(desired.Issue)
		if err != nil {
			return nil, err
		}
		filters, err := legacyIssueFilters(desired.Issue)
		if err != nil {
			return nil, err
		}
		actions, err := a.legacyIssueActions(ctx, organization, desired)
		if err != nil {
			return nil, err
		}
		owner, err := a.legacyOwner(ctx, organization, desired.OwnerTeam)
		if err != nil {
			return nil, err
		}
		frequency := int(desired.ThrottleMinutes)
		return legacyIssuePayload{
			Name: name, ActionMatch: defaultMatch(desired.Issue.TriggerMatch, "any"), FilterMatch: defaultMatch(desired.Issue.FilterMatch, "all"),
			Conditions: conditions, Filters: filters, Actions: actions, Frequency: frequency,
			Environment: desired.Issue.Environment, Owner: owner,
		}, nil
	}
	if adapter != LegacyMetricAdapter || desired.Metric == nil {
		return nil, ErrLegacyInvalidState
	}
	dataset, aggregate, query, eventTypes := legacyMetricSource(desired.Metric.Preset)
	if aggregate == "" {
		return nil, ErrLegacyInvalidState
	}
	detection := desired.Metric.Detection
	thresholdType := 0
	if detection.Direction == "below" {
		thresholdType = 1
	}
	actions, err := a.legacyMetricActions(ctx, organization, desired)
	if err != nil {
		return nil, err
	}
	triggers := []legacyMetricTrigger{{Label: "critical", AlertThreshold: detection.High, Actions: actions["high"]}}
	if detection.Medium != nil {
		triggers = append(triggers, legacyMetricTrigger{Label: "warning", AlertThreshold: *detection.Medium, Actions: actions["medium"]})
	}
	resolve := float64(0)
	if detection.Resolve != nil {
		resolve = *detection.Resolve
	}
	owner, err := a.legacyOwner(ctx, organization, desired.OwnerTeam)
	if err != nil {
		return nil, err
	}
	var environment *string
	if desired.Metric.Environment != "" {
		environment = &desired.Metric.Environment
	}
	payload := legacyMetricPayload{
		Name: name, QueryType: legacyQueryType(dataset), Dataset: dataset, Aggregate: aggregate, Query: query,
		TimeWindow: int(desired.Metric.IntervalMinutes), ResolveThreshold: resolve, ThresholdType: thresholdType,
		ThresholdPeriod: 1, Projects: []string{projectSlug}, EventTypes: eventTypes, Triggers: triggers,
		Environment: environment, Owner: owner, Description: desired.Description,
	}
	if detection.Kind == "change" {
		payload.DetectionType = "percent"
		payload.ComparisonDelta = detection.CompareMinutes * 60
	}
	return payload, nil
}

func legacyQueryType(dataset string) int {
	if dataset == "events" {
		return 0
	}
	return 1
}

func legacyIssueConditions(issue *IssueDesired) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(issue.Triggers))
	for _, trigger := range issue.Triggers {
		id := map[Trigger]string{
			TriggerFirstSeen:  "sentry.rules.conditions.first_seen_event.FirstSeenEventCondition",
			TriggerRegression: "sentry.rules.conditions.regression_event.RegressionEventCondition",
			TriggerEscalation: "sentry.rules.conditions.escalating_event.EscalatingEventCondition",
			TriggerEventCount: "sentry.rules.conditions.event_frequency.EventFrequencyCondition",
			TriggerUserCount:  "sentry.rules.conditions.event_frequency.EventUniqueUserFrequencyCondition",
			TriggerSessions:   "sentry.rules.conditions.event_frequency.EventFrequencyPercentCondition",
		}[trigger.Type]
		if id == "" {
			return nil, ErrLegacyInvalidState
		}
		condition := map[string]any{"id": id}
		if trigger.WindowMinutes != 0 {
			condition["interval"] = legacyInterval(trigger.WindowMinutes)
			condition["value"] = trigger.Threshold
		}
		result = append(result, condition)
	}
	return result, nil
}

func legacyIssueFilters(issue *IssueDesired) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(issue.Filters))
	for _, filter := range issue.Filters {
		id := map[string]string{
			"age":             "sentry.rules.filters.age_comparison.AgeComparisonFilter",
			"assignment":      "sentry.rules.filters.assigned_to.AssignedToFilter",
			"category":        "sentry.rules.filters.issue_category.IssueCategoryFilter",
			"frequency":       "sentry.rules.filters.issue_occurrences.IssueOccurrencesFilter",
			"event-attribute": "sentry.rules.filters.event_attribute.EventAttributeFilter",
			"tag":             "sentry.rules.filters.tagged_event.TaggedEventFilter",
			"latest-release":  "sentry.rules.filters.latest_release.LatestReleaseFilter",
			"level":           "sentry.rules.filters.level.LevelFilter",
		}[filter.Type]
		if id == "" {
			return nil, ErrLegacyInvalidState
		}
		payload := map[string]any{"id": id}
		switch filter.Type {
		case "tag":
			payload["match"], payload["key"], payload["value"] = filter.Operator, filter.Key, filter.Value
		case "event-attribute":
			payload["match"], payload["attribute"], payload["value"] = filter.Operator, filter.Key, filter.Value
		case "level":
			level, ok := legacyLevel(filter.Value)
			if !ok {
				return nil, ErrLegacyInvalidState
			}
			payload["match"], payload["level"] = filter.Operator, level
		default:
			payload["comparison_type"], payload["key"], payload["value"] = filter.Operator, filter.Key, filter.Value
		}
		result = append(result, payload)
	}
	return result, nil
}

func legacyLevel(value string) (string, bool) {
	level, ok := map[string]string{"fatal": "50", "error": "40", "warning": "30", "info": "20", "debug": "10", "sample": "0"}[value]
	return level, ok
}

func (a *LegacyHTTPAdapter) legacyIssueActions(ctx context.Context, organization string, desired Desired) ([]legacyIssueAction, error) {
	result := make([]legacyIssueAction, 0, len(desired.Notify)+len(desired.Actions))
	compatibility, err := a.destinationActions(LegacyIssueAdapter, desired.Notify)
	if err != nil {
		return nil, err
	}
	result = append(result, compatibility.([]legacyIssueAction)...)
	for _, action := range desired.Actions {
		if action.EmailTarget != "" {
			targetType, targetID, targetErr := a.legacyEmailTarget(ctx, organization, action)
			if targetErr != nil {
				return nil, targetErr
			}
			result = append(result, legacyIssueAction{ID: "sentry.mail.actions.NotifyEmailAction", TargetType: targetType, TargetIdentifier: targetID})
			continue
		}
		converted, destinationErr := a.destinationActions(LegacyIssueAdapter, []string{action.Destination})
		if destinationErr != nil {
			return nil, destinationErr
		}
		items := converted.([]legacyIssueAction)
		if len(items) == 1 && items[0].Severity != "" && action.PagerDutySeverity != "" {
			items[0].Severity = action.PagerDutySeverity
		}
		result = append(result, items...)
	}
	return result, nil
}

func (a *LegacyHTTPAdapter) legacyMetricActions(ctx context.Context, organization string, desired Desired) (map[string][]legacyMetricAction, error) {
	result := map[string][]legacyMetricAction{"high": {}, "medium": {}}
	compatibility, err := a.destinationActions(LegacyMetricAdapter, desired.Notify)
	if err != nil {
		return nil, err
	}
	result["high"] = append(result["high"], compatibility.([]legacyMetricAction)...)
	for _, action := range desired.Actions {
		status := action.Status
		if status == "" || status == "triggered" {
			status = "high"
		}
		if status != "high" && status != "medium" {
			return nil, ErrLegacyInvalidState
		}
		if action.EmailTarget != "" {
			if action.EmailTarget != "team" {
				return nil, ErrLegacyDestinationUnsupported
			}
			targetID, targetErr := a.legacyTeamID(ctx, organization, action.EmailTeam)
			if targetErr != nil {
				return nil, targetErr
			}
			result[status] = append(result[status], legacyMetricAction{Type: "email", TargetType: "team", TargetIdentifier: targetID})
			continue
		}
		converted, destinationErr := a.destinationActions(LegacyMetricAdapter, []string{action.Destination})
		if destinationErr != nil {
			return nil, destinationErr
		}
		items := converted.([]legacyMetricAction)
		if len(items) == 1 && items[0].Type == "pagerduty" {
			items[0].Severity = action.PagerDutySeverity
		}
		result[status] = append(result[status], items...)
	}
	return result, nil
}

func (a *LegacyHTTPAdapter) legacyEmailTarget(ctx context.Context, organization string, action ActionDesired) (string, any, error) {
	if action.EmailTarget == "suggested-assignees" {
		return "IssueOwners", nil, nil
	}
	if action.EmailTarget != "team" {
		return "", nil, ErrLegacyDestinationUnsupported
	}
	id, err := a.legacyTeamID(ctx, organization, action.EmailTeam)
	return "Team", id, err
}

func (a *LegacyHTTPAdapter) legacyOwner(ctx context.Context, organization, slug string) (string, error) {
	if slug == "" {
		return "", nil
	}
	id, err := a.legacyTeamID(ctx, organization, slug)
	if err != nil {
		return "", err
	}
	return "team:" + id, nil
}

func (a *LegacyHTTPAdapter) legacyTeamID(ctx context.Context, organization, slug string) (string, error) {
	if organization == "" {
		return "", ErrLegacyDestinationUnsupported
	}
	type team struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	items, err := sentryapi.CollectPages[team](ctx, a.Client, organizationPath(organization)+"teams/?query="+url.QueryEscape(slug), "list-legacy-teams")
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if item.Slug == slug && item.ID != "" {
			return item.ID, nil
		}
	}
	return "", ErrLegacyDestinationUnsupported
}

func legacyMetricSource(preset string) (string, string, string, []string) {
	switch preset {
	case "error-count":
		return "events", "count()", "", []string{"error"}
	case "affected-users":
		return "events", "count_unique(user)", "", []string{"error"}
	case "crash-free-sessions":
		return "sessions", "percentage(sessions_crashed, sessions)", "", []string{}
	case "crash-free-users":
		return "sessions", "percentage(users_crashed, users)", "", []string{}
	case "throughput":
		return "transactions", "count()", "", []string{"transaction"}
	case "duration":
		return "transactions", "p95(transaction.duration)", "", []string{"transaction"}
	case "failure-rate":
		return "transactions", "failure_rate()", "", []string{"transaction"}
	case "largest-contentful-paint":
		return "transactions", "p75(measurements.lcp)", "", []string{"transaction"}
	default:
		return "", "", "", nil
	}
}

func legacyInterval(minutes int32) string {
	return strconv.FormatInt(int64(minutes), 10) + "m"
}

func defaultMatch(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
