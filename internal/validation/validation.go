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

// Package validation implements cross-field CRD validation without requiring Kubernetes CEL
// admission support. Every function is pure and must run before an external Sentry API call.
package validation

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

// CronMonitor validates the independent Cron configuration before project or credentials are resolved.
func CronMonitor(spec sentryv1alpha1.SentryCronMonitorSpec) error {
	if spec.ProjectRef == "" || len(spec.ProjectRef) > 253 {
		return invalid("spec.projectRef", "is required")
	}
	if len(spec.DisplayName) > 128 {
		return invalid("spec.displayName", "must contain at most 128 characters")
	}
	if (spec.Crontab == nil) == (spec.Interval == nil) {
		return invalid("spec", "exactly one of crontab or interval is required")
	}
	if spec.Crontab != nil {
		if err := validateFiveFieldCron(spec.Crontab.Expression); err != nil {
			return invalid("spec.crontab.expression", err.Error())
		}
		zone := spec.Crontab.Timezone
		if zone == "" {
			zone = "UTC"
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return invalid("spec.crontab.timezone", "must be an IANA timezone")
		}
	}
	if spec.Interval != nil {
		if spec.Interval.Value < 1 || spec.Interval.Value > 10000 {
			return invalid("spec.interval.value", "must be between 1 and 10000")
		}
		switch spec.Interval.Unit {
		case "minute", "hour", "day", "week", "month", "year":
		default:
			return invalid("spec.interval.unit", "is unsupported")
		}
	}
	for _, bound := range []struct {
		field string
		value *int32
		max   int32
	}{
		{field: "spec.checkInMarginMinutes", value: spec.CheckInMarginMinutes, max: 10080},
		{field: "spec.maxRuntimeMinutes", value: spec.MaxRuntimeMinutes, max: 43200},
		{field: "spec.failureThreshold", value: spec.FailureThreshold, max: 1000},
		{field: "spec.recoveryThreshold", value: spec.RecoveryThreshold, max: 1000},
	} {
		if bound.value != nil && (*bound.value < 1 || *bound.value > bound.max) {
			return invalid(bound.field, fmt.Sprintf("must be between 1 and %d", bound.max))
		}
	}
	if spec.OwnerTeam != "" && (len(spec.OwnerTeam) > 64 || !dnsLabelPattern.MatchString(spec.OwnerTeam)) {
		return invalid("spec.ownerTeam", "must be a valid Sentry team slug")
	}
	if spec.OnDelete != "" && spec.OnDelete != sentryv1alpha1.MonitorDelete && spec.OnDelete != sentryv1alpha1.MonitorRetain {
		return invalid("spec.onDelete", "must be Delete or Retain")
	}
	return nil
}

// UptimeMonitor validates the public, non-secret surface before any Secret or Sentry access.
func UptimeMonitor(spec sentryv1alpha1.SentryUptimeMonitorSpec) error {
	if spec.ProjectRef == "" || len(spec.ProjectRef) > 253 {
		return invalid("spec.projectRef", "is required")
	}
	if len(spec.DisplayName) > 128 {
		return invalid("spec.displayName", "must contain at most 128 characters")
	}
	if len(spec.URL) > 255 {
		return invalid("spec.url", "must contain at most 255 characters")
	}
	parsed, err := url.Parse(spec.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return invalid("spec.url", "must be an absolute HTTP or HTTPS URL without userinfo or fragment")
	}
	if parsed.Port() != "" {
		port, err := strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return invalid("spec.url", "contains an invalid port")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return invalid("spec.url", "local destinations are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()) {
		return invalid("spec.url", "non-public IP destinations are not allowed")
	}
	switch spec.Method {
	case "", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return invalid("spec.method", "is unsupported")
	}
	switch spec.IntervalSeconds {
	case 60, 300, 600, 1200, 1800, 3600:
	default:
		return invalid("spec.intervalSeconds", "is unsupported")
	}
	if spec.TimeoutMilliseconds < 1000 || spec.TimeoutMilliseconds > 60000 {
		return invalid("spec.timeoutMilliseconds", "must be between 1000 and 60000")
	}
	if spec.FailureThreshold < 0 || spec.FailureThreshold > 1000 {
		return invalid("spec.failureThreshold", "must be zero or between 1 and 1000")
	}
	if spec.RecoveryThreshold < 0 || spec.RecoveryThreshold > 1000 {
		return invalid("spec.recoveryThreshold", "must be zero or between 1 and 1000")
	}
	if len(spec.Environment) > 64 {
		return invalid("spec.environment", "must contain at most 64 characters")
	}
	if spec.OwnerTeam != "" && (len(spec.OwnerTeam) > 64 || !dnsLabelPattern.MatchString(spec.OwnerTeam)) {
		return invalid("spec.ownerTeam", "must be a valid Sentry team slug")
	}
	if len(spec.Headers) > 20 {
		return invalid("spec.headers", "must contain at most 20 entries")
	}
	if len(spec.Assertions) > 10 {
		return invalid("spec.assertions", "must contain at most 10 entries")
	}
	if (spec.Method == "" || spec.Method == "GET" || spec.Method == "HEAD" || spec.Method == "OPTIONS") && spec.BodyFrom != nil {
		return invalid("spec.bodyFrom", "is not valid for a method without a request body")
	}
	headerNames := map[string]bool{}
	for i, header := range spec.Headers {
		name := strings.ToLower(strings.TrimSpace(header.Name))
		if name == "" || strings.ContainsAny(name, "\r\n:") {
			return invalid(fmt.Sprintf("spec.headers[%d].name", i), "is invalid")
		}
		if name == "host" || name == "authorization" || name == "proxy-authorization" || name == "cookie" {
			return invalid(fmt.Sprintf("spec.headers[%d].name", i), "is reserved or credential-bearing")
		}
		if headerNames[name] {
			return invalid(fmt.Sprintf("spec.headers[%d].name", i), "is duplicated")
		}
		headerNames[name] = true
		if header.ValueFrom.Name != "sentry-uptime-inputs" || header.ValueFrom.Key == "" {
			return invalid(fmt.Sprintf("spec.headers[%d].valueFrom", i), "must reference a key in sentry-uptime-inputs")
		}
	}
	if spec.BodyFrom != nil && (spec.BodyFrom.Name != "sentry-uptime-inputs" || spec.BodyFrom.Key == "") {
		return invalid("spec.bodyFrom", "must reference a key in sentry-uptime-inputs")
	}
	for i, assertion := range spec.Assertions {
		if len(assertion.Key) > 256 {
			return invalid(fmt.Sprintf("spec.assertions[%d].key", i), "must contain at most 256 characters")
		}
		if assertion.Value == "" || len(assertion.Value) > 512 {
			return invalid(fmt.Sprintf("spec.assertions[%d].value", i), "must contain between 1 and 512 characters")
		}
		switch assertion.Type {
		case "statusCode":
			if assertion.Key != "" {
				return invalid(fmt.Sprintf("spec.assertions[%d].key", i), "must be empty for statusCode")
			}
			code, err := strconv.Atoi(assertion.Value)
			if err != nil || code < 100 || code > 599 {
				return invalid(fmt.Sprintf("spec.assertions[%d].value", i), "must be an HTTP status code")
			}
		case "responseHeader":
			if strings.TrimSpace(assertion.Key) == "" || strings.ContainsAny(assertion.Key, "\r\n:") {
				return invalid(fmt.Sprintf("spec.assertions[%d].key", i), "must be a response header name")
			}
		case "jsonPath":
			if !strings.HasPrefix(assertion.Key, "$") || len(assertion.Key) > 256 || strings.ContainsAny(assertion.Key, "\r\n") {
				return invalid(fmt.Sprintf("spec.assertions[%d].key", i), "must be a bounded JSON path")
			}
		default:
			return invalid(fmt.Sprintf("spec.assertions[%d].type", i), "is unsupported")
		}
		switch assertion.Comparison {
		case "equals", "notEquals", "lessThan", "greaterThan":
		default:
			return invalid(fmt.Sprintf("spec.assertions[%d].comparison", i), "is unsupported")
		}
	}
	if spec.OnDelete != "" && spec.OnDelete != sentryv1alpha1.MonitorDelete && spec.OnDelete != sentryv1alpha1.MonitorRetain {
		return invalid("spec.onDelete", "must be Delete or Retain")
	}
	return nil
}

func validateFiveFieldCron(expression string) error {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return fmt.Errorf("must contain exactly five fields")
	}
	for _, field := range fields {
		if field == "" || !regexp.MustCompile(`^[0-9*/?,\-]+$`).MatchString(field) {
			return fmt.Errorf("contains unsupported syntax")
		}
		if strings.Contains(field, "?") {
			return fmt.Errorf("question-mark syntax is not supported")
		}
	}
	return nil
}

var (
	dnsLabelPattern      = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)
	localKeyPattern      = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	builtinSymbolSources = map[sentryv1alpha1.BuiltinSymbolSource]bool{
		"amd": true, "autodesk": true, "chromium": true, "citrix": true, "electron": true,
		"intel": true, "microsoft": true, "mozilla": true, "nuget": true, "nvidia": true,
		"ubuntu": true, "unity": true,
	}
)

// Error is a safe, stable validation failure. It contains no credentials or external IDs.
type Error struct {
	Field   string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

func invalid(field, message string) error { return &Error{Field: field, Message: message} }

// Connection validates relationships that were formerly expressed with CRD CEL rules.
func Connection(spec sentryv1alpha1.SentryConnectionSpec) error {
	https := strings.HasPrefix(spec.URL, "https://")
	if !https && (spec.Transport == nil || !spec.Transport.AllowInsecureHTTP) {
		return invalid("spec.url", "plain HTTP requires transport.allowInsecureHTTP")
	}
	if spec.Transport != nil && spec.Transport.InsecureSkipTLSVerify {
		if !https {
			return invalid("spec.transport.insecureSkipTLSVerify", "requires an HTTPS URL")
		}
		if spec.Credentials.CABundleSecretRef != nil {
			return invalid("spec.transport.insecureSkipTLSVerify", "cannot be combined with credentials.caBundleSecretRef")
		}
	}
	if spec.Credentials.CABundleSecretRef != nil && !https {
		return invalid("spec.credentials.caBundleSecretRef", "requires an HTTPS URL")
	}
	if spec.ProjectManagement.UnmanagedProjects == sentryv1alpha1.UnmanagedProjectsAdopt &&
		(spec.Ownership == nil || spec.Ownership.Role != sentryv1alpha1.ConnectionRoleActive) {
		return invalid("spec.projectManagement.unmanagedProjects", "Adopt requires ownership.role Active")
	}
	return nil
}

// Project validates all cross-field relationships before project reconciliation performs I/O.
func Project(spec sentryv1alpha1.SentryProjectSpec) error {
	if spec.AlertPolicy != "" && len(spec.AlertPolicies) > 0 {
		return invalid("spec.alertPolicies", "cannot be combined with alertPolicy")
	}
	seenPolicies := map[string]bool{}
	for index, name := range spec.AlertPolicies {
		if name == "" || seenPolicies[name] {
			return invalid(fmt.Sprintf("spec.alertPolicies[%d]", index), "must be non-empty and unique")
		}
		seenPolicies[name] = true
	}
	if spec.Team == "" {
		return invalid("spec.team", "is required")
	}
	if err := validateDSN(spec); err != nil {
		return err
	}
	if err := validateSettings(spec.Settings); err != nil {
		return err
	}
	if err := validateSymbolSources(spec.SymbolSources); err != nil {
		return err
	}
	if err := validateCodeMappings(spec.CodeMappings); err != nil {
		return err
	}
	if err := validateProjectTeams(spec); err != nil {
		return err
	}
	if err := validateAlerts(spec.Alerts); err != nil {
		return err
	}
	if spec.NotificationTargets != nil {
		if _, err := spec.NotificationTargets.Flatten(); err != nil {
			return invalid("spec.notificationTargets", "target names must be unique within each provider")
		}
	}
	return nil
}

// AlertPolicy validates reusable alert content without resolving any consuming project.
func AlertPolicy(spec sentryv1alpha1.SentryAlertPolicySpec) error {
	if spec.NotificationTargets != nil {
		if _, err := spec.NotificationTargets.Flatten(); err != nil {
			return invalid("spec.notificationTargets", "target names must be unique within each provider")
		}
	}
	return validateAlerts(&spec.Alerts)
}

func validateDSN(spec sentryv1alpha1.SentryProjectSpec) error {
	publishers := 0
	if spec.ClientKeys != nil {
		for name, key := range *spec.ClientKeys {
			if !key.PublishDSN {
				continue
			}
			publishers++
			if !key.Active {
				return invalid("spec.clientKeys."+name+".publishDSN", "requires an active managed key")
			}
		}
	}
	if publishers > 1 {
		return invalid("spec.clientKeys", "at most one managed key may publish its public DSN")
	}
	if spec.ExistingClientKey != nil {
		if !spec.ExistingClientKey.PublishDSN {
			return invalid("spec.existingClientKey.publishDSN", "must be true when existingClientKey is declared")
		}
		if publishers != 0 {
			return invalid("spec.existingClientKey", "cannot be combined with publishDSN on a managed client key")
		}
	}
	return nil
}

func validateSettings(settings *sentryv1alpha1.ProjectSettings) error {
	if settings == nil {
		return nil
	}
	if settings.PII != nil && settings.PII.AdvancedRules != nil {
		for ruleIndex, rule := range *settings.PII.AdvancedRules {
			if rule.Dataset == "" {
				rule.Dataset = sentryv1alpha1.AdvancedPIIDataset("events")
			}
			replace := rule.Redaction == sentryv1alpha1.AdvancedPIIRedaction("replace")
			if replace != (rule.Replacement != "") {
				return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].replacement", ruleIndex), "is required only for replace redaction")
			}
			for selectorIndex, selector := range rule.Applications {
				pathForbidden := selector.Root == sentryv1alpha1.AdvancedPIISelectorRoot("message") || selector.Root == sentryv1alpha1.AdvancedPIISelectorRoot("log-body")
				if pathForbidden == (len(selector.Path) > 0) || selector.Root == sentryv1alpha1.AdvancedPIISelectorRoot("metric-attribute") && len(selector.Path) != 1 {
					return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].applications[%d].path", ruleIndex, selectorIndex), "message and log-body selectors forbid a path; metric attributes require one segment; other roots require a path")
				}
				switch rule.Dataset {
				case sentryv1alpha1.AdvancedPIIDataset("events"):
					if selector.Root == sentryv1alpha1.AdvancedPIISelectorRoot("log-body") || selector.Root == sentryv1alpha1.AdvancedPIISelectorRoot("metric-attribute") {
						return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].applications[%d].root", ruleIndex, selectorIndex), "selector does not belong to the events dataset")
					}
				case sentryv1alpha1.AdvancedPIIDataset("logs"):
					if selector.Root != sentryv1alpha1.AdvancedPIISelectorRoot("log-body") {
						return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].applications[%d].root", ruleIndex, selectorIndex), "logs rules require log-body selectors")
					}
				case sentryv1alpha1.AdvancedPIIDataset("application-metrics"):
					if selector.Root != sentryv1alpha1.AdvancedPIISelectorRoot("metric-attribute") {
						return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].applications[%d].root", ruleIndex, selectorIndex), "application-metrics rules require metric-attribute selectors")
					}
				}
			}
		}
	}
	if settings.Grouping != nil && settings.Grouping.Fingerprints != nil {
		for index, rule := range *settings.Grouping.Fingerprints {
			if !rule.IncludeDefault && len(rule.Values) == 0 {
				return invalid(fmt.Sprintf("spec.settings.grouping.fingerprints[%d]", index), "requires a custom value or includeDefault")
			}
		}
	}
	if settings.Behavior != nil && settings.Behavior.AllowedDomains != nil {
		domains := *settings.Behavior.AllowedDomains
		if len(domains) > 1 {
			for _, domain := range domains {
				if domain == "*" {
					return invalid("spec.settings.behavior.allowedDomains", "wildcard must be the only entry")
				}
			}
		}
	}
	if settings.Notifications != nil && settings.Notifications.MinimumDigestMinutes != nil && settings.Notifications.MaximumDigestMinutes != nil && *settings.Notifications.MinimumDigestMinutes > *settings.Notifications.MaximumDigestMinutes {
		return invalid("spec.settings.notifications", "minimumDigestMinutes cannot exceed maximumDigestMinutes")
	}
	if settings.Symbolication != nil && settings.Symbolication.BuiltinSources != nil {
		for _, source := range *settings.Symbolication.BuiltinSources {
			if !builtinSymbolSources[source] {
				return invalid("spec.settings.symbolication.builtinSources", "contains an unsupported source")
			}
		}
	}
	return nil
}

func validateSymbolSources(sources *map[string]sentryv1alpha1.CustomSymbolSourceSpec) error {
	if sources == nil {
		return nil
	}
	for name, source := range *sources {
		if !localKeyPattern.MatchString(name) {
			return invalid("spec.symbolSources", "keys must be lowercase DNS labels no longer than 32 characters")
		}
		types := 0
		if source.HTTP != nil {
			types++
			if !strings.HasPrefix(source.HTTP.URL, "https://") {
				return invalid("spec.symbolSources."+name+".http.url", "must use HTTPS")
			}
		}
		if source.S3 != nil {
			types++
		}
		if source.GCS != nil {
			types++
		}
		if types != 1 {
			return invalid("spec.symbolSources."+name, "exactly one of http, s3 or gcs is required")
		}
	}
	return nil
}

func validateCodeMappings(mappings *map[string]sentryv1alpha1.CodeMappingSpec) error {
	if mappings == nil {
		return nil
	}
	for name, mapping := range *mappings {
		if mapping.ImportCodeowners && mapping.DefaultBranch == "" {
			return invalid("spec.codeMappings."+name+".defaultBranch", "is required when importCodeowners is enabled")
		}
	}
	return nil
}

func validateProjectTeams(spec sentryv1alpha1.SentryProjectSpec) error {
	if spec.ClientKeys != nil {
		for name := range *spec.ClientKeys {
			if !localKeyPattern.MatchString(name) {
				return invalid("spec.clientKeys", "keys must be lowercase DNS labels no longer than 32 characters")
			}
		}
	}
	attached := map[string]bool{spec.Team: true}
	if spec.GrantAccess != nil {
		for _, team := range spec.GrantAccess.Teams {
			if attached[team] {
				return invalid("spec.grantAccess.teams", "must not repeat the primary project team")
			}
			attached[team] = true
		}
	}
	if spec.IssueOwnership != nil {
		for ruleIndex, rule := range spec.IssueOwnership.Rules {
			matcher := rule.Matcher
			if (matcher.Type == sentryv1alpha1.IssueOwnershipMatcherTag) != (matcher.TagKey != "") {
				return invalid(fmt.Sprintf("spec.issueOwnership.rules[%d].matcher.tagKey", ruleIndex), "is required only for tag matchers")
			}
			for _, team := range rule.Teams {
				if !attached[team] {
					return invalid(fmt.Sprintf("spec.issueOwnership.rules[%d].teams", ruleIndex), "teams must be primary or declared in grantAccess")
				}
			}
		}
	}
	if spec.Alerts != nil {
		for name, alert := range *spec.Alerts {
			if alert.OwnerTeam != "" && !attached[alert.OwnerTeam] {
				return invalid("spec.alerts."+name+".ownerTeam", "must be primary or declared in grantAccess")
			}
			for index, action := range alert.Actions {
				if action.Email != nil && action.Email.Target == sentryv1alpha1.EmailTarget("team") && !attached[action.Email.Team] {
					return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].email.team", name, index), "must be primary or declared in grantAccess")
				}
			}
		}
	}
	return nil
}

func validateAlerts(alerts *map[string]sentryv1alpha1.AlertSpec) error {
	if alerts == nil {
		return nil
	}
	for name, alert := range *alerts {
		if !dnsLabelPattern.MatchString(name) {
			return invalid("spec.alerts", "keys must be lowercase DNS labels")
		}
		typed := 0
		if alert.Issue != nil {
			typed++
		}
		if alert.Metric != nil {
			typed++
		}
		legacy := alert.Trigger != ""
		if typed > 1 || legacy && typed != 0 || !legacy && typed == 0 {
			return invalid("spec.alerts."+name, "select exactly one compatibility trigger, issue block, or metric block")
		}
		if legacy {
			metric := alert.Trigger == sentryv1alpha1.AlertTriggerEventCount || alert.Trigger == sentryv1alpha1.AlertTriggerUserCount
			if metric != (alert.WindowMinutes != nil && alert.Threshold != nil) {
				return invalid("spec.alerts."+name, "metric triggers require windowMinutes and threshold; issue triggers forbid them")
			}
		}
		if alert.Issue != nil {
			if alert.WindowMinutes != nil || alert.Threshold != nil {
				return invalid("spec.alerts."+name, "typed issue alerts forbid compatibility metric fields")
			}
			if alert.Issue.TriggerMatch == "" {
				alert.Issue.TriggerMatch = sentryv1alpha1.MatchMode("any")
			}
			if alert.Issue.FilterMatch == "" {
				alert.Issue.FilterMatch = sentryv1alpha1.MatchMode("all")
			}
			for index, trigger := range alert.Issue.Triggers {
				counted := trigger.Type == sentryv1alpha1.IssueTriggerType("event-count") || trigger.Type == sentryv1alpha1.IssueTriggerType("user-count") || trigger.Type == sentryv1alpha1.IssueTriggerType("session-percentage")
				if counted != (trigger.WindowMinutes != nil && trigger.Threshold != nil) {
					return invalid(fmt.Sprintf("spec.alerts.%s.issue.triggers[%d]", name, index), "counted triggers require windowMinutes and threshold; state triggers forbid them")
				}
			}
			for index, filter := range alert.Issue.Filters {
				if filter.Value == "" {
					return invalid(fmt.Sprintf("spec.alerts.%s.issue.filters[%d].value", name, index), "must not be empty")
				}
				keyRequired := filter.Type == sentryv1alpha1.IssueFilterType("tag") || filter.Type == sentryv1alpha1.IssueFilterType("event-attribute") || filter.Type == sentryv1alpha1.IssueFilterType("release-age")
				if keyRequired != (filter.Key != "") {
					return invalid(fmt.Sprintf("spec.alerts.%s.issue.filters[%d].key", name, index), "is required only for tag, event-attribute, and release-age filters")
				}
				if filter.Type == sentryv1alpha1.IssueFilterType("assignment") && filter.Value != "unassigned" {
					return invalid(fmt.Sprintf("spec.alerts.%s.issue.filters[%d].value", name, index), "only unassigned is currently supported")
				}
			}
		}
		if alert.Metric != nil {
			metric := alert.Metric
			if (metric.Preset == sentryv1alpha1.MetricPreset("application-metric")) != (metric.ApplicationMetric != nil) {
				return invalid("spec.alerts."+name+".metric.applicationMetric", "is required only for the application-metric preset")
			}
			if metric.ApplicationMetric != nil {
				if err := validateApplicationMetricExpression(*metric.ApplicationMetric); err != nil {
					return invalid("spec.alerts."+name+".metric.applicationMetric", err.Error())
				}
			}
			detections := 0
			if metric.Detection.Static != nil {
				detections++
			}
			if metric.Detection.Change != nil {
				detections++
			}
			if metric.Detection.Dynamic != nil {
				detections++
			}
			if detections != 1 {
				return invalid("spec.alerts."+name+".metric.detection", "select exactly one detection mode")
			}
			if metric.Detection.Static != nil {
				if err := validateThresholds(metric.Detection.Static.Direction, float64(metric.Detection.Static.High), float64Pointer(metric.Detection.Static.Medium), float64Pointer(metric.Detection.Static.Resolve)); err != nil {
					return invalid("spec.alerts."+name+".metric.detection.static", err.Error())
				}
			}
			if metric.Detection.Change != nil {
				change := metric.Detection.Change
				if err := validateChangeThresholds(float64(change.HighPercent), float64Pointer(change.MediumPercent), float64Pointer(change.ResolvePercent)); err != nil {
					return invalid("spec.alerts."+name+".metric.detection.change", err.Error())
				}
			}
		}
		if len(alert.Notify) == 0 && len(alert.Actions) == 0 {
			return invalid("spec.alerts."+name, "at least one notify target or typed action is required")
		}
		for index, action := range alert.Actions {
			selected := 0
			if action.Destination != "" {
				selected++
			}
			if action.Email != nil {
				selected++
			}
			if action.GitHubIssue != nil {
				selected++
			}
			if selected != 1 {
				return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d]", name, index), "select exactly one destination, email, or GitHub issue action")
			}
			if action.Email != nil && (action.Email.Target == sentryv1alpha1.EmailTarget("team")) != (action.Email.Team != "") {
				return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].email.team", name, index), "is required only for team email targets")
			}
			if action.PagerDutySeverity != "" && !strings.HasPrefix(action.Destination, "pagerDuty/") {
				return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].pagerDutySeverity", name, index), "is valid only for PagerDuty destinations")
			}
			if action.GitHubIssue != nil && alert.Issue == nil {
				return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].githubIssue", name, index), "is valid only for issue workflows")
			}
		}
	}
	return nil
}

func float64Pointer(value *int64) *float64 {
	if value == nil {
		return nil
	}
	converted := float64(*value)
	return &converted
}

func validateThresholds(direction sentryv1alpha1.ThresholdDirection, high float64, medium, resolve *float64) error {
	if high < 0 {
		return fmt.Errorf("high threshold must not be negative")
	}
	if medium != nil && direction == sentryv1alpha1.ThresholdDirection("above") && *medium > high {
		return fmt.Errorf("medium threshold cannot exceed high threshold for above detection")
	}
	if medium != nil && direction == sentryv1alpha1.ThresholdDirection("below") && *medium < high {
		return fmt.Errorf("medium threshold cannot be below high threshold for below detection")
	}
	if resolve != nil && *resolve < 0 {
		return fmt.Errorf("resolve threshold must not be negative")
	}
	return nil
}

func validateChangeThresholds(high float64, medium, resolve *float64) error {
	if high < 0 {
		return fmt.Errorf("high percentage must not be negative")
	}
	if medium != nil && *medium > high {
		return fmt.Errorf("medium percentage cannot exceed high percentage")
	}
	if medium != nil && *medium < 0 {
		return fmt.Errorf("medium percentage must not be negative")
	}
	if resolve != nil && *resolve < 0 {
		return fmt.Errorf("resolve percentage must not be negative")
	}
	return nil
}

// ProjectCompatibility rejects version-specific fields before adapters or Sentry I/O.
func ProjectCompatibility(spec sentryv1alpha1.SentryProjectSpec, profile sentryv1alpha1.AlertCompatibilityProfile) error {
	modern := profile == sentryv1alpha1.AlertCompatibilityModern26
	if spec.Settings != nil {
		if filters := spec.Settings.Filters; !modern && filters != nil && (filters.LogMessages != nil || filters.ApplicationMetrics != nil) {
			return invalid("spec.settings.filters", "log and application-metric filters require modern-26.10.1")
		}
		if pii := spec.Settings.PII; !modern && pii != nil && pii.AdvancedRules != nil {
			for index, rule := range *pii.AdvancedRules {
				if rule.Dataset != "" && rule.Dataset != sentryv1alpha1.AdvancedPIIDataset("events") {
					return invalid(fmt.Sprintf("spec.settings.pii.advancedRules[%d].dataset", index), "requires modern-26.10.1")
				}
			}
		}
		if !modern && spec.Settings.Behavior != nil && spec.Settings.Behavior.ReleaseAutoCreation != nil {
			return invalid("spec.settings.behavior.releaseAutoCreation", "requires modern-26.10.1")
		}
	}
	if !modern && spec.ClientKeys != nil {
		for name, key := range *spec.ClientKeys {
			if key.Loader != nil && (key.Loader.LogsAndMetrics != nil || key.Loader.UserFeedback != nil || key.Loader.Debug != nil) {
				return invalid("spec.clientKeys."+name+".loader", "logs, metrics, feedback, and debug switches require modern-26.10.1")
			}
		}
	}
	if spec.Alerts != nil {
		for name, alert := range *spec.Alerts {
			if !modern && alert.Issue != nil {
				for index, trigger := range alert.Issue.Triggers {
					if trigger.Type == sentryv1alpha1.IssueTriggerType("resolved") {
						return invalid(fmt.Sprintf("spec.alerts.%s.issue.triggers[%d]", name, index), "requires modern-26.10.1")
					}
				}
				for index, filter := range alert.Issue.Filters {
					if filter.Type == sentryv1alpha1.IssueFilterType("priority") || filter.Type == sentryv1alpha1.IssueFilterType("type") || filter.Type == sentryv1alpha1.IssueFilterType("release-age") {
						return invalid(fmt.Sprintf("spec.alerts.%s.issue.filters[%d]", name, index), "requires modern-26.10.1")
					}
				}
			}
			if !modern && alert.Metric != nil && (alert.Metric.Detection.Dynamic != nil || alert.Metric.Preset == sentryv1alpha1.MetricPreset("logs") || alert.Metric.Preset == sentryv1alpha1.MetricPreset("application-metric")) {
				return invalid("spec.alerts."+name+".metric", "selected preset or detection mode requires modern-26.10.1")
			}
			if !modern {
				for index, action := range alert.Actions {
					if action.GitHubIssue != nil {
						return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].githubIssue", name, index), "requires modern-26.10.1")
					}
					if action.Status == sentryv1alpha1.AlertActionStatus("resolved") {
						return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].status", name, index), "resolved actions require modern-26.10.1")
					}
					if alert.Metric != nil && action.Email != nil && action.Email.Target == sentryv1alpha1.EmailTarget("suggested-assignees") {
						return invalid(fmt.Sprintf("spec.alerts.%s.actions[%d].email.target", name, index), "suggested-assignees for metric alerts requires modern-26.10.1")
					}
				}
			}
		}
	}
	return nil
}

func validateApplicationMetricExpression(expression sentryv1alpha1.ApplicationMetricExpression) error {
	if err := validateApplicationMetricTerm(expression.Left); err != nil {
		return fmt.Errorf("left: %w", err)
	}
	if (expression.Operator == "") != (expression.Right == nil) {
		return fmt.Errorf("operator and right must be supplied together")
	}
	if expression.Right != nil {
		if err := validateApplicationMetricTerm(*expression.Right); err != nil {
			return fmt.Errorf("right: %w", err)
		}
	}
	return nil
}

func validateApplicationMetricTerm(term sentryv1alpha1.ApplicationMetricTerm) error {
	allowed := map[sentryv1alpha1.ApplicationMetricType]map[sentryv1alpha1.ApplicationMetricAggregation]bool{
		"counter":      {"sum": true, "per-second": true, "per-minute": true},
		"gauge":        {"count": true, "sum": true, "avg": true, "min": true, "max": true, "per-second": true, "per-minute": true},
		"distribution": {"count": true, "sum": true, "avg": true, "min": true, "max": true, "p50": true, "p75": true, "p90": true, "p95": true, "p99": true, "per-second": true, "per-minute": true},
	}
	if !allowed[term.Type][term.Aggregation] {
		return fmt.Errorf("aggregation %q is not valid for %s metrics", term.Aggregation, term.Type)
	}
	return nil
}
