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

// Package settings reconciles only the source-confirmed typed project settings.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

var (
	ErrInvalidDesiredState = errors.New("project settings desired state is invalid")
	ErrUnsupportedFilter   = errors.New("required inbound filter is unavailable")
)

type Authorization func(context.Context) error

type Filter struct {
	ID                string
	Active            bool
	Subfilters        []string
	SubfiltersPresent bool
}

func (f *Filter) UnmarshalJSON(payload []byte) error {
	var wire struct {
		ID     string          `json:"id"`
		Active json.RawMessage `json:"active"`
	}
	if json.Unmarshal(payload, &wire) != nil || wire.ID == "" || len(wire.Active) == 0 {
		return ErrUnsupportedFilter
	}
	var active *bool
	if err := json.Unmarshal(wire.Active, &active); err == nil && active != nil {
		*f = Filter{ID: wire.ID, Active: *active}
		return nil
	}
	var subfilters []string
	if err := json.Unmarshal(wire.Active, &subfilters); err != nil || subfilters == nil {
		return ErrUnsupportedFilter
	}
	*f = Filter{ID: wire.ID, Subfilters: subfilters, SubfiltersPresent: true}
	return nil
}

type FilterPatch struct {
	Active     *bool     `json:"active,omitempty"`
	Subfilters *[]string `json:"subfilters,omitempty"`
}

type ProjectState struct {
	ScrubData            *bool
	ScrubDefaults        *bool
	ScrubIPAddresses     *bool
	SensitiveFields      *[]string
	SafeFields           *[]string
	ResolveAge           *int64
	DefaultEnvironment   *string
	StoreCrashReports    *int32
	ScrapeJavaScript     *bool
	AllowedDomains       *[]string
	RelayPIIConfig       *string
	GroupingEnhancements *string
	FingerprintingRules  *string
	BuiltinSymbolSources *[]string
	ReleaseAutoCreation  *bool
	SubjectPrefix        *string
	SubjectTemplate      *string
	DigestsMinDelay      *int32
	DigestsMaxDelay      *int32
	Options              map[string]any
	ETag                 string
}

type ProjectPatch struct {
	ScrubData            *bool          `json:"dataScrubber,omitempty"`
	ScrubDefaults        *bool          `json:"dataScrubberDefaults,omitempty"`
	ScrubIPAddresses     *bool          `json:"scrubIPAddresses,omitempty"`
	SensitiveFields      *[]string      `json:"sensitiveFields,omitempty"`
	SafeFields           *[]string      `json:"safeFields,omitempty"`
	ResolveAge           *int64         `json:"resolveAge,omitempty"`
	DefaultEnvironment   *string        `json:"defaultEnvironment,omitempty"`
	StoreCrashReports    *int32         `json:"storeCrashReports,omitempty"`
	ScrapeJavaScript     *bool          `json:"scrapeJavaScript,omitempty"`
	AllowedDomains       *[]string      `json:"allowedDomains,omitempty"`
	RelayPIIConfig       *string        `json:"relayPiiConfig,omitempty"`
	GroupingEnhancements *string        `json:"groupingEnhancements,omitempty"`
	FingerprintingRules  *string        `json:"fingerprintingRules,omitempty"`
	BuiltinSymbolSources *[]string      `json:"builtinSymbolSources,omitempty"`
	ReleaseAutoCreation  *bool          `json:"enableAutoReleaseCreation,omitempty"`
	SubjectPrefix        *string        `json:"subjectPrefix,omitempty"`
	SubjectTemplate      *string        `json:"subjectTemplate,omitempty"`
	DigestsMinDelay      *int32         `json:"digestsMinDelay,omitempty"`
	DigestsMaxDelay      *int32         `json:"digestsMaxDelay,omitempty"`
	Options              map[string]any `json:"options,omitempty"`
}

func (p ProjectPatch) Empty() bool {
	return p.ScrubData == nil && p.ScrubDefaults == nil && p.ScrubIPAddresses == nil && p.SensitiveFields == nil && p.SafeFields == nil &&
		p.ResolveAge == nil && p.DefaultEnvironment == nil && p.StoreCrashReports == nil && p.ScrapeJavaScript == nil && p.AllowedDomains == nil &&
		p.RelayPIIConfig == nil && p.GroupingEnhancements == nil && p.FingerprintingRules == nil && p.BuiltinSymbolSources == nil &&
		p.ReleaseAutoCreation == nil && p.SubjectPrefix == nil && p.SubjectTemplate == nil && p.DigestsMinDelay == nil && p.DigestsMaxDelay == nil && len(p.Options) == 0
}

type Adapter interface {
	ListFilters(context.Context, string, string) ([]Filter, error)
	UpdateFilter(context.Context, string, string, string, FilterPatch) error
	GetProjectState(context.Context, string, string) (ProjectState, error)
	UpdateProjectState(context.Context, string, string, ProjectPatch, string) error
}

type Input struct {
	Organization string
	ProjectSlug  string
	Desired      *sentryv1alpha1.ProjectSettings
	Modern       bool
	Authorize    Authorization
}

type Component struct {
	Managed bool
	Changed int
	Err     error
}

type Result struct {
	Filters       Component
	PII           Component
	Behavior      Component
	Grouping      Component
	Symbolication Component
	Notifications Component
}

func (r Result) Err() error {
	return errors.Join(r.Filters.Err, r.PII.Err, r.Behavior.Err, r.Grouping.Err, r.Symbolication.Err, r.Notifications.Err)
}

type Engine struct {
	Adapter Adapter
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{}
	if e.Adapter == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" {
		result.Filters.Err = ErrInvalidDesiredState
		return result
	}
	if input.Desired == nil {
		return result
	}
	if err := validate(input.Desired); err != nil {
		if input.Desired.Filters != nil {
			result.Filters = Component{Managed: true, Err: err}
		}
		if input.Desired.PII != nil {
			result.PII = Component{Managed: true, Err: err}
		}
		if input.Desired.Behavior != nil {
			result.Behavior = Component{Managed: true, Err: err}
		}
		if input.Desired.Grouping != nil {
			result.Grouping = Component{Managed: true, Err: err}
		}
		if input.Desired.Symbolication != nil {
			result.Symbolication = Component{Managed: true, Err: err}
		}
		if input.Desired.Notifications != nil {
			result.Notifications = Component{Managed: true, Err: err}
		}
		return result
	}
	if input.Desired.Filters != nil {
		result.Filters.Managed = true
		result.Filters.Changed, result.Filters.Err = e.reconcileFilters(ctx, input, input.Desired.Filters)
	}
	if input.Desired.PII != nil {
		result.PII.Managed = true
		result.PII.Changed, result.PII.Err = e.reconcilePII(ctx, input, input.Desired.PII)
	}
	if input.Desired.Behavior != nil {
		result.Behavior.Managed = true
		result.Behavior.Changed, result.Behavior.Err = e.reconcileBehavior(ctx, input, input.Desired.Behavior)
	}
	if input.Desired.Grouping != nil {
		result.Grouping.Managed = true
		result.Grouping.Changed, result.Grouping.Err = e.reconcileGrouping(ctx, input, input.Desired.Grouping)
	}
	if input.Desired.Symbolication != nil {
		result.Symbolication.Managed = true
		result.Symbolication.Changed, result.Symbolication.Err = e.reconcileSymbolication(ctx, input, input.Desired.Symbolication)
	}
	if input.Desired.Notifications != nil {
		result.Notifications.Managed = true
		result.Notifications.Changed, result.Notifications.Err = e.reconcileNotifications(ctx, input, input.Desired.Notifications)
	}
	return result
}

func (e *Engine) reconcileBehavior(ctx context.Context, input Input, desired *sentryv1alpha1.BehaviorSettings) (int, error) {
	if desired.AutoResolveHours == nil && desired.DefaultEnvironment == nil && desired.CrashReportLimit == nil && desired.ScrapeJavaScript == nil && desired.AllowedDomains == nil && desired.ReleaseAutoCreation == nil {
		return 0, nil
	}
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	patch := ProjectPatch{}
	changed := 0
	if desired.AutoResolveHours != nil && !equalInt64(current.ResolveAge, desired.AutoResolveHours) {
		patch.ResolveAge = desired.AutoResolveHours
		changed++
	}
	if desired.DefaultEnvironment != nil && !equalDefaultEnvironment(current.DefaultEnvironment, desired.DefaultEnvironment) {
		patch.DefaultEnvironment = desired.DefaultEnvironment
		changed++
	}
	if desired.CrashReportLimit != nil && !equalInt32(current.StoreCrashReports, desired.CrashReportLimit) {
		patch.StoreCrashReports = desired.CrashReportLimit
		changed++
	}
	if desired.ScrapeJavaScript != nil && !equalBool(current.ScrapeJavaScript, desired.ScrapeJavaScript) {
		patch.ScrapeJavaScript = desired.ScrapeJavaScript
		changed++
	}
	if desired.AllowedDomains != nil {
		wanted := append([]string(nil), (*desired.AllowedDomains)...)
		sort.Strings(wanted)
		if current.AllowedDomains == nil || !equalStringSets(*current.AllowedDomains, wanted) {
			patch.AllowedDomains = &wanted
			changed++
		}
	}
	if desired.ReleaseAutoCreation != nil && !equalBool(current.ReleaseAutoCreation, desired.ReleaseAutoCreation) {
		patch.ReleaseAutoCreation = desired.ReleaseAutoCreation
		changed++
	}
	if patch.Empty() {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, patch, current.ETag); err != nil {
		return 0, err
	}
	return changed, nil
}

func (e *Engine) reconcileFilters(ctx context.Context, input Input, desired *sentryv1alpha1.FilterSettings) (int, error) {
	filters, err := e.Adapter.ListFilters(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	current := make(map[string]bool, len(filters))
	for _, filter := range filters {
		if filter.ID == "" {
			return 0, ErrUnsupportedFilter
		}
		if _, duplicate := current[filter.ID]; duplicate {
			return 0, ErrUnsupportedFilter
		}
		current[filter.ID] = filter.Active
	}
	wanted := []struct {
		id    string
		value *bool
	}{
		{id: "browser-extensions", value: desired.BrowserExtensions},
		{id: "localhost", value: desired.Localhost},
		{id: "web-crawlers", value: desired.WebCrawlers},
		{id: "filtered-transaction", value: desired.FilteredTransactions},
	}
	changed := 0
	for _, item := range wanted {
		if item.value == nil {
			continue
		}
		active, found := current[item.id]
		if !found {
			return changed, fmt.Errorf("%w: %s", ErrUnsupportedFilter, item.id)
		}
		if active == *item.value {
			continue
		}
		if err := input.Authorize(ctx); err != nil {
			return changed, err
		}
		value := *item.value
		if err := e.Adapter.UpdateFilter(ctx, input.Organization, input.ProjectSlug, item.id, FilterPatch{Active: &value}); err != nil {
			return changed, err
		}
		changed++
	}
	if desired.LegacyBrowsers != nil {
		observed, found := findFilter(filters, "legacy-browsers")
		if !found {
			return changed, fmt.Errorf("%w: legacy-browsers", ErrUnsupportedFilter)
		}
		desiredSubfilters := canonicalLegacyBrowserFilters(*desired.LegacyBrowsers)
		if !legacyBrowserStateEqual(observed, desiredSubfilters) {
			if err := input.Authorize(ctx); err != nil {
				return changed, err
			}
			if err := e.Adapter.UpdateFilter(ctx, input.Organization, input.ProjectSlug, "legacy-browsers", FilterPatch{Subfilters: &desiredSubfilters}); err != nil {
				return changed, err
			}
			changed++
		}
	}
	optionPatch, optionChanges, etag, err := filterOptionsPatch(ctx, e.Adapter, input, desired)
	if err != nil {
		return changed, err
	}
	if optionChanges > 0 {
		if err := input.Authorize(ctx); err != nil {
			return changed, err
		}
		if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, optionPatch, etag); err != nil {
			return changed, err
		}
		changed += optionChanges
	}
	return changed, nil
}

func filterOptionsPatch(ctx context.Context, adapter Adapter, input Input, desired *sentryv1alpha1.FilterSettings) (ProjectPatch, int, string, error) {
	if desired.HydrationErrors == nil && desired.ChunkLoadErrors == nil && desired.IPAddresses == nil && desired.Releases == nil && desired.ErrorMessages == nil && desired.LogMessages == nil && desired.ApplicationMetrics == nil {
		return ProjectPatch{}, 0, "", nil
	}
	current, err := adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return ProjectPatch{}, 0, "", err
	}
	patch := ProjectPatch{Options: map[string]any{}}
	changed := 0
	setBool := func(key string, value *bool) {
		if value == nil {
			return
		}
		observed, ok := current.Options[key].(bool)
		if !ok || observed != *value {
			patch.Options[key] = *value
			changed++
		}
	}
	setList := func(key string, values *[]string) {
		if values == nil {
			return
		}
		canonical := append([]string(nil), (*values)...)
		sort.Strings(canonical)
		wanted := strings.Join(canonical, "\n")
		observed, ok := current.Options[key].(string)
		if !ok || observed != wanted {
			patch.Options[key] = wanted
			changed++
		}
	}
	setBool("filters:react-hydration-errors", desired.HydrationErrors)
	setBool("filters:chunk-load-error", desired.ChunkLoadErrors)
	setList("filters:blacklisted_ips", desired.IPAddresses)
	setList("filters:releases", desired.Releases)
	setList("filters:error_messages", desired.ErrorMessages)
	setList("filters:log_messages", desired.LogMessages)
	setList("filters:trace_metric_names", desired.ApplicationMetrics)
	if changed == 0 {
		patch.Options = nil
	}
	return patch, changed, current.ETag, nil
}

func findFilter(filters []Filter, id string) (Filter, bool) {
	for _, filter := range filters {
		if filter.ID == id {
			return filter, true
		}
	}
	return Filter{}, false
}

func canonicalLegacyBrowserFilters(values []sentryv1alpha1.LegacyBrowserFilter) []string {
	order := []sentryv1alpha1.LegacyBrowserFilter{
		sentryv1alpha1.LegacyBrowserIE, sentryv1alpha1.LegacyBrowserEdge,
		sentryv1alpha1.LegacyBrowserSafari, sentryv1alpha1.LegacyBrowserFirefox,
		sentryv1alpha1.LegacyBrowserChrome, sentryv1alpha1.LegacyBrowserOpera,
		sentryv1alpha1.LegacyBrowserAndroid, sentryv1alpha1.LegacyBrowserOperaMini,
	}
	wanted := make(map[sentryv1alpha1.LegacyBrowserFilter]bool, len(values))
	for _, value := range values {
		wanted[value] = true
	}
	result := make([]string, 0, len(wanted))
	for _, value := range order {
		if wanted[value] {
			result = append(result, string(value))
		}
	}
	return result
}

func legacyBrowserStateEqual(current Filter, desired []string) bool {
	if current.SubfiltersPresent {
		return equalStringSets(current.Subfilters, desired)
	}
	return !current.Active && len(desired) == 0
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]bool, len(left))
	for _, value := range left {
		if values[value] {
			return false
		}
		values[value] = true
	}
	for _, value := range right {
		if !values[value] {
			return false
		}
	}
	return true
}

func (e *Engine) reconcilePII(ctx context.Context, input Input, desired *sentryv1alpha1.PIISettings) (int, error) {
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	patch := ProjectPatch{}
	changed := 0
	if desired.ScrubData != nil && !equalBool(current.ScrubData, desired.ScrubData) {
		patch.ScrubData = desired.ScrubData
		changed++
	}
	if desired.ScrubDefaults != nil && !equalBool(current.ScrubDefaults, desired.ScrubDefaults) {
		patch.ScrubDefaults = desired.ScrubDefaults
		changed++
	}
	if desired.ScrubIPAddresses != nil && !equalBool(current.ScrubIPAddresses, desired.ScrubIPAddresses) {
		patch.ScrubIPAddresses = desired.ScrubIPAddresses
		changed++
	}
	if desired.SensitiveFields != nil && !equalStrings(current.SensitiveFields, desired.SensitiveFields) {
		patch.SensitiveFields = desired.SensitiveFields
		changed++
	}
	if desired.SafeFields != nil && !equalStrings(current.SafeFields, desired.SafeFields) {
		patch.SafeFields = desired.SafeFields
		changed++
	}
	if desired.AdvancedRules != nil {
		serialized, err := serializeAdvancedPIIRules(*desired.AdvancedRules)
		if err != nil {
			return 0, err
		}
		if !equalJSONString(current.RelayPIIConfig, serialized) {
			patch.RelayPIIConfig = &serialized
			changed++
		}
	}
	if patch.Empty() {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, patch, current.ETag); err != nil {
		return 0, err
	}
	return changed, nil
}

func (e *Engine) reconcileNotifications(ctx context.Context, input Input, desired *sentryv1alpha1.ProjectNotificationSettings) (int, error) {
	if desired.SubjectPrefix == nil && desired.SubjectTemplate == nil && desired.MinimumDigestMinutes == nil && desired.MaximumDigestMinutes == nil {
		return 0, nil
	}
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	patch := ProjectPatch{}
	changed := 0
	if desired.SubjectPrefix != nil && !equalString(current.SubjectPrefix, desired.SubjectPrefix) {
		patch.SubjectPrefix = desired.SubjectPrefix
		changed++
	}
	if desired.SubjectTemplate != nil && !equalString(current.SubjectTemplate, desired.SubjectTemplate) {
		patch.SubjectTemplate = desired.SubjectTemplate
		changed++
	}
	if desired.MinimumDigestMinutes != nil {
		seconds := *desired.MinimumDigestMinutes * 60
		if !equalInt32(current.DigestsMinDelay, &seconds) {
			patch.DigestsMinDelay = &seconds
			changed++
		}
	}
	if desired.MaximumDigestMinutes != nil {
		seconds := *desired.MaximumDigestMinutes * 60
		if !equalInt32(current.DigestsMaxDelay, &seconds) {
			patch.DigestsMaxDelay = &seconds
			changed++
		}
	}
	if patch.Empty() {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, patch, current.ETag); err != nil {
		return 0, err
	}
	return changed, nil
}

func (e *Engine) reconcileGrouping(ctx context.Context, input Input, desired *sentryv1alpha1.GroupingSettings) (int, error) {
	if desired.Enhancements == nil && desired.Fingerprints == nil {
		return 0, nil
	}
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	patch := ProjectPatch{}
	changed := 0
	if desired.Enhancements != nil {
		serialized, serializeErr := serializeGroupingEnhancements(*desired.Enhancements)
		if serializeErr != nil {
			return 0, serializeErr
		}
		if !equalString(current.GroupingEnhancements, &serialized) {
			patch.GroupingEnhancements = &serialized
			changed++
		}
	}
	if desired.Fingerprints != nil {
		serialized, serializeErr := serializeFingerprintRules(*desired.Fingerprints)
		if serializeErr != nil {
			return 0, serializeErr
		}
		if !equalString(current.FingerprintingRules, &serialized) {
			patch.FingerprintingRules = &serialized
			changed++
		}
	}
	if patch.Empty() {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, patch, current.ETag); err != nil {
		return 0, err
	}
	return changed, nil
}

func (e *Engine) reconcileSymbolication(ctx context.Context, input Input, desired *sentryv1alpha1.SymbolicationSettings) (int, error) {
	if desired.BuiltinSources == nil {
		return 0, nil
	}
	current, err := e.Adapter.GetProjectState(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return 0, err
	}
	wanted := canonicalBuiltinSymbolSources(*desired.BuiltinSources)
	if current.BuiltinSymbolSources != nil && equalStringSets(*current.BuiltinSymbolSources, wanted) {
		return 0, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return 0, err
	}
	if err := e.Adapter.UpdateProjectState(ctx, input.Organization, input.ProjectSlug, ProjectPatch{BuiltinSymbolSources: &wanted}, current.ETag); err != nil {
		return 0, err
	}
	return 1, nil
}

func validate(desired *sentryv1alpha1.ProjectSettings) error {
	if desired.Behavior != nil {
		behavior := desired.Behavior
		if behavior.AutoResolveHours != nil && (*behavior.AutoResolveHours < 0 || *behavior.AutoResolveHours > 2147483647) {
			return ErrInvalidDesiredState
		}
		if behavior.DefaultEnvironment != nil && (utf8.RuneCountInString(*behavior.DefaultEnvironment) > 64 || strings.ContainsAny(*behavior.DefaultEnvironment, "\r\n\f/")) {
			return ErrInvalidDesiredState
		}
		if behavior.CrashReportLimit != nil && (*behavior.CrashReportLimit < -1 || *behavior.CrashReportLimit > 100) {
			return ErrInvalidDesiredState
		}
		if behavior.AllowedDomains != nil && !validAllowedDomains(*behavior.AllowedDomains) {
			return ErrInvalidDesiredState
		}
	}
	if desired.Filters != nil && desired.Filters.LegacyBrowsers != nil {
		allowed := map[sentryv1alpha1.LegacyBrowserFilter]bool{
			sentryv1alpha1.LegacyBrowserIE: true, sentryv1alpha1.LegacyBrowserEdge: true,
			sentryv1alpha1.LegacyBrowserSafari: true, sentryv1alpha1.LegacyBrowserFirefox: true,
			sentryv1alpha1.LegacyBrowserChrome: true, sentryv1alpha1.LegacyBrowserOpera: true,
			sentryv1alpha1.LegacyBrowserAndroid: true, sentryv1alpha1.LegacyBrowserOperaMini: true,
		}
		seen := make(map[sentryv1alpha1.LegacyBrowserFilter]bool, len(*desired.Filters.LegacyBrowsers))
		for _, value := range *desired.Filters.LegacyBrowsers {
			if !allowed[value] || seen[value] {
				return ErrInvalidDesiredState
			}
			seen[value] = true
		}
	}
	if desired.PII != nil {
		for _, list := range []*[]string{desired.PII.SensitiveFields, desired.PII.SafeFields} {
			if list == nil {
				continue
			}
			seen := make(map[string]struct{}, len(*list))
			for _, value := range *list {
				if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
					return ErrInvalidDesiredState
				}
				if _, duplicate := seen[value]; duplicate {
					return ErrInvalidDesiredState
				}
				seen[value] = struct{}{}
			}
		}
		if desired.PII.AdvancedRules != nil {
			if _, err := serializeAdvancedPIIRules(*desired.PII.AdvancedRules); err != nil {
				return ErrInvalidDesiredState
			}
		}
	}
	if desired.Grouping != nil {
		if desired.Grouping.Enhancements != nil {
			if _, err := serializeGroupingEnhancements(*desired.Grouping.Enhancements); err != nil {
				return ErrInvalidDesiredState
			}
		}
		if desired.Grouping.Fingerprints != nil {
			if _, err := serializeFingerprintRules(*desired.Grouping.Fingerprints); err != nil {
				return ErrInvalidDesiredState
			}
		}
	}
	if desired.Symbolication != nil && desired.Symbolication.BuiltinSources != nil && !validBuiltinSymbolSources(*desired.Symbolication.BuiltinSources) {
		return ErrInvalidDesiredState
	}
	return nil
}

func validAllowedDomains(values []string) bool {
	if len(values) == 0 || len(values) > 100 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "*" {
			if len(values) != 1 {
				return false
			}
			continue
		}
		if len(value) == 0 || len(value) > 253 || strings.ContainsAny(value, "\r\n\t ?#@") {
			return false
		}
		candidate := value
		if !strings.Contains(candidate, "://") {
			candidate = "https://" + candidate
		}
		parsed, err := url.Parse(candidate)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return false
		}
		hostname := strings.TrimPrefix(parsed.Hostname(), "*.")
		if hostname == "" || !validDNSHostname(hostname) {
			return false
		}
		if port := parsed.Port(); port != "" {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return false
			}
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validDNSHostname(value string) bool {
	if len(value) > 253 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character != '-' && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
				return false
			}
		}
	}
	return true
}

func canonicalBuiltinSymbolSources(values []sentryv1alpha1.BuiltinSymbolSource) []string {
	order := []sentryv1alpha1.BuiltinSymbolSource{
		"amd", "autodesk", "chromium", "citrix", "electron", "intel", "microsoft", "mozilla", "nuget", "nvidia", "ubuntu", "unity",
	}
	wanted := make(map[sentryv1alpha1.BuiltinSymbolSource]bool, len(values))
	for _, value := range values {
		wanted[value] = true
	}
	result := make([]string, 0, len(values))
	for _, value := range order {
		if wanted[value] {
			result = append(result, string(value))
		}
	}
	return result
}

func validBuiltinSymbolSources(values []sentryv1alpha1.BuiltinSymbolSource) bool {
	if len(values) > 12 {
		return false
	}
	allowed := map[sentryv1alpha1.BuiltinSymbolSource]bool{
		"amd": true, "autodesk": true, "chromium": true, "citrix": true, "electron": true, "intel": true,
		"microsoft": true, "mozilla": true, "nuget": true, "nvidia": true, "ubuntu": true, "unity": true,
	}
	seen := make(map[sentryv1alpha1.BuiltinSymbolSource]bool, len(values))
	for _, value := range values {
		if !allowed[value] || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func equalBool(current, desired *bool) bool {
	return current != nil && desired != nil && *current == *desired
}

func equalInt64(current, desired *int64) bool {
	return current != nil && desired != nil && *current == *desired
}

func equalInt32(current, desired *int32) bool {
	return current != nil && desired != nil && *current == *desired
}

func equalDefaultEnvironment(current, desired *string) bool {
	if desired == nil {
		return current == nil
	}
	if *desired == "" {
		return current == nil || *current == ""
	}
	return current != nil && *current == *desired
}

func equalString(current, desired *string) bool {
	return current != nil && desired != nil && *current == *desired
}

func equalStrings(current, desired *[]string) bool {
	return current != nil && desired != nil && reflect.DeepEqual(*current, *desired)
}
