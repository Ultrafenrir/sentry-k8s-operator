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

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestFilterDecodesBooleanOrLegacySubfilterStateOnly(t *testing.T) {
	t.Parallel()

	var boolean Filter
	if err := json.Unmarshal([]byte(`{"id":"filtered-transaction","active":true}`), &boolean); err != nil || !boolean.Active || boolean.SubfiltersPresent {
		t.Fatalf("boolean=%#v err=%v", boolean, err)
	}
	var legacy Filter
	if err := json.Unmarshal([]byte(`{"id":"legacy-browsers","active":["ie","chrome"]}`), &legacy); err != nil || legacy.Active || !legacy.SubfiltersPresent || !reflect.DeepEqual(legacy.Subfilters, []string{"ie", "chrome"}) {
		t.Fatalf("legacy=%#v err=%v", legacy, err)
	}
	for _, payload := range []string{`{"id":"legacy-browsers","active":null}`, `{"id":"legacy-browsers"}`, `{"id":"legacy-browsers","active":{}}`} {
		if err := json.Unmarshal([]byte(payload), &Filter{}); !errors.Is(err, ErrUnsupportedFilter) {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
}

func TestEngineReconcilesOnlyExplicitFieldsAndWholeLists(t *testing.T) {
	t.Parallel()

	trueValue := true
	falseValue := false
	empty := []string{}
	adapter := &fakeAdapter{
		filters: []Filter{
			{ID: "browser-extensions", Active: false},
			{ID: "localhost", Active: true},
			{ID: "web-crawlers", Active: false},
		},
		state: ProjectState{
			ScrubData:       &falseValue,
			SensitiveFields: stringSlicePointer([]string{"manual-old"}),
			SafeFields:      stringSlicePointer([]string{"manual-neighbor"}),
			ETag:            `"revision-7"`,
		},
	}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue, Localhost: &falseValue},
			PII:     &sentryv1alpha1.PIISettings{ScrubData: &trueValue, SensitiveFields: &empty},
		},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Filters.Changed != 2 || result.PII.Changed != 2 || authorizations != 3 {
		t.Fatalf("result=%#v authorizations=%d", result, authorizations)
	}
	if !reflect.DeepEqual(adapter.filterUpdates, []filterUpdate{
		{id: "browser-extensions", patch: activeFilterPatch(true)},
		{id: "localhost", patch: activeFilterPatch(false)},
	}) {
		t.Fatalf("filter updates = %#v", adapter.filterUpdates)
	}
	if len(adapter.projectUpdates) != 1 {
		t.Fatalf("project updates = %#v", adapter.projectUpdates)
	}
	patch := adapter.projectUpdates[0]
	if patch.etag != `"revision-7"` || patch.patch.ScrubData == nil || !*patch.patch.ScrubData || patch.patch.SensitiveFields == nil || len(*patch.patch.SensitiveFields) != 0 {
		t.Fatalf("patch = %#v", patch)
	}
	if patch.patch.SafeFields != nil || patch.patch.ScrubDefaults != nil || patch.patch.ScrubIPAddresses != nil {
		t.Fatalf("unmanaged neighboring fields were included: %#v", patch.patch)
	}
}

func TestEngineReconcilesFilteredTransactionsAndLegacyBrowserSubfilters(t *testing.T) {
	t.Parallel()

	trueValue := true
	legacy := []sentryv1alpha1.LegacyBrowserFilter{
		sentryv1alpha1.LegacyBrowserChrome,
		sentryv1alpha1.LegacyBrowserIE,
	}
	adapter := &fakeAdapter{filters: []Filter{
		{ID: "filtered-transaction", Active: false},
		{ID: "legacy-browsers", Subfilters: []string{"firefox", "ie"}, SubfiltersPresent: true},
	}}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{Filters: &sentryv1alpha1.FilterSettings{
			FilteredTransactions: &trueValue,
			LegacyBrowsers:       &legacy,
		}},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if result.Err() != nil || result.Filters.Changed != 2 || authorizations != 2 {
		t.Fatalf("result=%#v authorizations=%d", result, authorizations)
	}
	desiredLegacy := []string{"ie", "chrome"}
	if !reflect.DeepEqual(adapter.filterUpdates, []filterUpdate{
		{id: "filtered-transaction", patch: activeFilterPatch(true)},
		{id: "legacy-browsers", patch: FilterPatch{Subfilters: &desiredLegacy}},
	}) {
		t.Fatalf("filter updates=%#v", adapter.filterUpdates)
	}
}

func TestEngineReconcilesBehaviorSettingsWithOneFreshNarrowPatch(t *testing.T) {
	t.Parallel()

	currentResolve, desiredResolve := int64(24), int64(72)
	currentEnvironment, desiredEnvironment := "staging", "production"
	currentCrashLimit, desiredCrashLimit := int32(0), int32(-1)
	currentScrape, desiredScrape := false, true
	currentDomains := []string{"staging.example.com"}
	desiredDomains := []string{"https://checkout.example.com:443", "*.checkout.example.com"}
	adapter := &fakeAdapter{state: ProjectState{
		ResolveAge: &currentResolve, DefaultEnvironment: &currentEnvironment,
		StoreCrashReports: &currentCrashLimit, ScrapeJavaScript: &currentScrape,
		AllowedDomains: &currentDomains,
		ETag:           `"behavior-4"`,
	}}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{Behavior: &sentryv1alpha1.BehaviorSettings{
			AutoResolveHours: &desiredResolve, DefaultEnvironment: &desiredEnvironment,
			CrashReportLimit: &desiredCrashLimit, ScrapeJavaScript: &desiredScrape,
			AllowedDomains: &desiredDomains,
		}},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if result.Err() != nil || result.Behavior.Changed != 5 || authorizations != 1 || adapter.reads != 2 || len(adapter.projectUpdates) != 1 {
		t.Fatalf("result=%#v authorizations=%d adapter=%#v", result, authorizations, adapter)
	}
	update := adapter.projectUpdates[0]
	if update.etag != `"behavior-4"` || update.patch.ResolveAge != &desiredResolve || update.patch.DefaultEnvironment != &desiredEnvironment ||
		update.patch.StoreCrashReports != &desiredCrashLimit || update.patch.ScrapeJavaScript != &desiredScrape ||
		update.patch.AllowedDomains == nil || !reflect.DeepEqual(*update.patch.AllowedDomains, []string{"*.checkout.example.com", "https://checkout.example.com:443"}) {
		t.Fatalf("update=%#v", update)
	}
	if update.patch.ScrubData != nil || update.patch.SensitiveFields != nil {
		t.Fatalf("unmanaged PII leaked into behavior patch: %#v", update.patch)
	}
}

func TestEngineReconcilesModernFiltersPIIDatasetsAndNotificationDefaults(t *testing.T) {
	t.Parallel()
	authorizations := 0
	adapter := &fakeAdapter{
		filters: []Filter{},
		state: ProjectState{RelayPIIConfig: stringPointer(`{"rules":{},"applications":{}}`), Options: map[string]any{
			"filters:react-hydration-errors": false, "filters:chunk-load-error": false,
			"filters:blacklisted_ips": "", "filters:releases": "", "filters:error_messages": "", "filters:log_messages": "", "filters:trace_metric_names": "",
		}},
	}
	on := true
	ips := []string{"10.0.0.0/8"}
	releases := []string{"legacy-*"}
	errors := []string{"ignore me"}
	logs := []string{"healthcheck"}
	metrics := []string{"internal.*"}
	rules := []sentryv1alpha1.AdvancedPIIRule{{Dataset: "logs", Type: "email", Redaction: "remove", Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "log-body"}}}}
	prefix := "[checkout]"
	template := "$shortID - $title"
	minimum, maximum := int32(5), int32(60)
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout", Modern: true,
		Desired: &sentryv1alpha1.ProjectSettings{
			Filters:       &sentryv1alpha1.FilterSettings{HydrationErrors: &on, ChunkLoadErrors: &on, IPAddresses: &ips, Releases: &releases, ErrorMessages: &errors, LogMessages: &logs, ApplicationMetrics: &metrics},
			PII:           &sentryv1alpha1.PIISettings{AdvancedRules: &rules},
			Notifications: &sentryv1alpha1.ProjectNotificationSettings{SubjectPrefix: &prefix, SubjectTemplate: &template, MinimumDigestMinutes: &minimum, MaximumDigestMinutes: &maximum},
		},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Filters.Changed != 7 || result.PII.Changed != 1 || result.Notifications.Changed != 4 || authorizations != 3 {
		t.Fatalf("result=%#v authorizations=%d", result, authorizations)
	}
	if len(adapter.projectUpdates) != 3 || len(adapter.projectUpdates[0].patch.Options) != 7 || adapter.projectUpdates[1].patch.RelayPIIConfig == nil || adapter.projectUpdates[2].patch.SubjectPrefix == nil || adapter.projectUpdates[2].patch.DigestsMinDelay == nil || *adapter.projectUpdates[2].patch.DigestsMinDelay != 300 {
		t.Fatalf("updates=%#v", adapter.projectUpdates)
	}
}

func TestEngineSerializesTypedAdvancedPIIAndGroupingRules(t *testing.T) {
	t.Parallel()

	advanced := []sentryv1alpha1.AdvancedPIIRule{
		{
			Type: "email", Redaction: "remove",
			Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "message"}, {Root: "extra", Path: []string{"customer", "email"}}},
		},
		{
			Type: "anything", Redaction: "replace", Replacement: "[Filtered]",
			Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "request", Path: []string{"headers", "Authorization"}}},
		},
	}
	enhancements := []sentryv1alpha1.GroupingEnhancementRule{
		{Matcher: "path", Pattern: "*/node_modules/*", Action: "MarkOutOfApp"},
		{Matcher: "function", Pattern: "vendor_*", Negate: true, Action: "Include"},
	}
	fingerprints := []sentryv1alpha1.FingerprintRule{
		{Matcher: "error.type", Pattern: "DatabaseUnavailable", Values: []string{"database-unavailable"}},
		{Matcher: "stack.function", Pattern: "query_*", Values: []string{"database"}, IncludeDefault: true},
	}
	adapter := &fakeAdapter{state: ProjectState{ETag: `"typed-3"`}}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			PII:      &sentryv1alpha1.PIISettings{AdvancedRules: &advanced},
			Grouping: &sentryv1alpha1.GroupingSettings{Enhancements: &enhancements, Fingerprints: &fingerprints},
		},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if result.Err() != nil || result.PII.Changed != 1 || result.Grouping.Changed != 2 || authorizations != 2 || len(adapter.projectUpdates) != 2 {
		t.Fatalf("result=%#v authorizations=%d updates=%#v", result, authorizations, adapter.projectUpdates)
	}
	pii := adapter.projectUpdates[0].patch.RelayPIIConfig
	if pii == nil || *pii != `{"rules":{"0":{"type":"email","redaction":{"method":"remove"}},"1":{"type":"anything","redaction":{"method":"replace","text":"[Filtered]"}}},"applications":{"$message":["0"],"extra.customer.email":["0"],"request.headers.Authorization":["1"]}}` {
		t.Fatalf("relay PII config=%v", pii)
	}
	grouping := adapter.projectUpdates[1].patch
	if grouping.GroupingEnhancements == nil || *grouping.GroupingEnhancements != "stack.abs_path:\"*/node_modules/*\" -app\n!stack.function:\"vendor_*\" +group" {
		t.Fatalf("enhancements=%v", grouping.GroupingEnhancements)
	}
	if grouping.FingerprintingRules == nil || *grouping.FingerprintingRules != "error.type:\"DatabaseUnavailable\" -> \"database-unavailable\"\nstack.function:\"query_*\" -> \"database\", {{ default }}" {
		t.Fatalf("fingerprints=%v", grouping.FingerprintingRules)
	}
	if grouping.RelayPIIConfig != nil || adapter.projectUpdates[0].patch.GroupingEnhancements != nil {
		t.Fatalf("independent component fields leaked across patches: %#v", adapter.projectUpdates)
	}
}

func TestEngineConvergesOnCanonicalTypedRuleReadback(t *testing.T) {
	t.Parallel()

	advanced := []sentryv1alpha1.AdvancedPIIRule{{
		Type: "ip", Redaction: "hash", Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "user", Path: []string{"ip_address"}}},
	}}
	enhancements := []sentryv1alpha1.GroupingEnhancementRule{{Matcher: "module", Pattern: "payments.*", Action: "Exclude"}}
	fingerprints := []sentryv1alpha1.FingerprintRule{{Matcher: "message", Pattern: "timeout *", IncludeDefault: true}}
	relay := `{"applications":{"user.ip_address":["0"]},"rules":{"0":{"redaction":{"method":"hash"},"type":"ip"}}}`
	enhancementText := `stack.module:"payments.*" -group`
	fingerprintText := `message:"timeout *" -> {{ default }}`
	adapter := &fakeAdapter{state: ProjectState{
		RelayPIIConfig: &relay, GroupingEnhancements: &enhancementText, FingerprintingRules: &fingerprintText,
	}}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			PII:      &sentryv1alpha1.PIISettings{AdvancedRules: &advanced},
			Grouping: &sentryv1alpha1.GroupingSettings{Enhancements: &enhancements, Fingerprints: &fingerprints},
		},
		Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Err() != nil || result.PII.Changed != 0 || result.Grouping.Changed != 0 || len(adapter.projectUpdates) != 0 {
		t.Fatalf("result=%#v updates=%#v", result, adapter.projectUpdates)
	}
}

func TestEngineRejectsDroppedGroupingAndReleaseAutoCreation(t *testing.T) {
	t.Parallel()

	enhancements := []sentryv1alpha1.GroupingEnhancementRule{{Matcher: "module", Pattern: "payments.*", Action: "Exclude"}}
	autoRelease := true
	adapter := &fakeAdapter{dropGrouping: true, dropReleaseAutoCreation: true}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			Behavior: &sentryv1alpha1.BehaviorSettings{ReleaseAutoCreation: &autoRelease},
			Grouping: &sentryv1alpha1.GroupingSettings{Enhancements: &enhancements},
		},
		Authorize: func(context.Context) error { return nil },
	})
	if !errors.Is(result.Behavior.Err, ErrVerification) || !errors.Is(result.Grouping.Err, ErrVerification) {
		t.Fatalf("dropped settings result=%#v", result)
	}
}

func TestEngineReconcilesPortableBuiltinSymbolSourcesAsASet(t *testing.T) {
	t.Parallel()

	current := []string{"microsoft"}
	desired := []sentryv1alpha1.BuiltinSymbolSource{"nuget", "microsoft", "electron"}
	adapter := &fakeAdapter{state: ProjectState{BuiltinSymbolSources: &current, ETag: `"symbols-2"`}}
	authorizations := 0
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired:   &sentryv1alpha1.ProjectSettings{Symbolication: &sentryv1alpha1.SymbolicationSettings{BuiltinSources: &desired}},
		Authorize: func(context.Context) error { authorizations++; return nil },
	})
	if result.Err() != nil || result.Symbolication.Changed != 1 || authorizations != 1 || len(adapter.projectUpdates) != 1 {
		t.Fatalf("result=%#v authorizations=%d updates=%#v", result, authorizations, adapter.projectUpdates)
	}
	update := adapter.projectUpdates[0]
	if update.etag != `"symbols-2"` || update.patch.BuiltinSymbolSources == nil || !reflect.DeepEqual(*update.patch.BuiltinSymbolSources, []string{"electron", "microsoft", "nuget"}) {
		t.Fatalf("update=%#v", update)
	}
	if update.patch.GroupingEnhancements != nil || update.patch.RelayPIIConfig != nil {
		t.Fatalf("unmanaged settings leaked into symbolication patch: %#v", update.patch)
	}
}

func TestEngineRejectsUnknownOrDuplicateBuiltinSymbolSourcesBeforeRead(t *testing.T) {
	t.Parallel()

	for _, values := range [][]sentryv1alpha1.BuiltinSymbolSource{{"proton"}, {"microsoft", "microsoft"}} {
		adapter := &fakeAdapter{}
		result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
			Organization: "company", ProjectSlug: "checkout",
			Desired:   &sentryv1alpha1.ProjectSettings{Symbolication: &sentryv1alpha1.SymbolicationSettings{BuiltinSources: &values}},
			Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
		})
		if !errors.Is(result.Err(), ErrInvalidDesiredState) || adapter.reads != 0 {
			t.Fatalf("values=%v result=%#v reads=%d", values, result, adapter.reads)
		}
	}
}

func TestTypedRuleSerializersRejectUnrepresentableInput(t *testing.T) {
	t.Parallel()

	invalidPII := []sentryv1alpha1.AdvancedPIIRule{{
		Type: "pattern", Redaction: "remove", Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "message"}},
	}}
	invalidGrouping := []sentryv1alpha1.GroupingEnhancementRule{{Matcher: "path", Pattern: "bad\npattern", Action: "Exclude"}}
	invalidFingerprint := []sentryv1alpha1.FingerprintRule{{Matcher: "message", Pattern: "timeout", Values: []string{"{{ default }}"}}}
	for name, desired := range map[string]*sentryv1alpha1.ProjectSettings{
		"pii":         {PII: &sentryv1alpha1.PIISettings{AdvancedRules: &invalidPII}},
		"grouping":    {Grouping: &sentryv1alpha1.GroupingSettings{Enhancements: &invalidGrouping}},
		"fingerprint": {Grouping: &sentryv1alpha1.GroupingSettings{Fingerprints: &invalidFingerprint}},
	} {
		t.Run(name, func(t *testing.T) {
			adapter := &fakeAdapter{}
			result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
				Organization: "company", ProjectSlug: "checkout", Desired: desired,
				Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
			})
			if !errors.Is(result.Err(), ErrInvalidDesiredState) || len(adapter.projectUpdates) != 0 {
				t.Fatalf("result=%#v updates=%#v", result, adapter.projectUpdates)
			}
		})
	}
}

func TestEngineTreatsMissingAndEmptyDefaultEnvironmentAsEquivalent(t *testing.T) {
	t.Parallel()

	empty := ""
	adapter := &fakeAdapter{state: ProjectState{DefaultEnvironment: nil}}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired:   &sentryv1alpha1.ProjectSettings{Behavior: &sentryv1alpha1.BehaviorSettings{DefaultEnvironment: &empty}},
		Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Err() != nil || result.Behavior.Changed != 0 || len(adapter.projectUpdates) != 0 {
		t.Fatalf("result=%#v updates=%#v", result, adapter.projectUpdates)
	}
}

func TestEngineRejectsInvalidBehaviorBeforeRead(t *testing.T) {
	t.Parallel()

	negativeResolve := int64(-1)
	tooManyReports := int32(101)
	invalidEnvironment := "production/internal"
	emptyDomains := []string{}
	mixedWildcardDomains := []string{"*", "example.com"}
	pathDomains := []string{"https://example.com/path"}
	invalidPortDomains := []string{"example.com:70000"}
	duplicateDomains := []string{"example.com", "example.com"}
	for _, behavior := range []*sentryv1alpha1.BehaviorSettings{
		{AutoResolveHours: &negativeResolve},
		{CrashReportLimit: &tooManyReports},
		{DefaultEnvironment: &invalidEnvironment},
		{AllowedDomains: &emptyDomains},
		{AllowedDomains: &mixedWildcardDomains},
		{AllowedDomains: &pathDomains},
		{AllowedDomains: &invalidPortDomains},
		{AllowedDomains: &duplicateDomains},
	} {
		adapter := &fakeAdapter{}
		result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
			Organization: "company", ProjectSlug: "checkout",
			Desired:   &sentryv1alpha1.ProjectSettings{Behavior: behavior},
			Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
		})
		if !errors.Is(result.Err(), ErrInvalidDesiredState) || adapter.reads != 0 {
			t.Fatalf("behavior=%#v result=%#v reads=%d", behavior, result, adapter.reads)
		}
	}
}

func TestEngineTreatsExplicitEmptyLegacyBrowserSetAsDisabled(t *testing.T) {
	t.Parallel()

	empty := []sentryv1alpha1.LegacyBrowserFilter{}
	adapter := &fakeAdapter{filters: []Filter{{ID: "legacy-browsers", Active: false}}}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company", ProjectSlug: "checkout",
		Desired:   &sentryv1alpha1.ProjectSettings{Filters: &sentryv1alpha1.FilterSettings{LegacyBrowsers: &empty}},
		Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Err() != nil || result.Filters.Changed != 0 || len(adapter.filterUpdates) != 0 {
		t.Fatalf("result=%#v updates=%#v", result, adapter.filterUpdates)
	}
}

func TestEngineRejectsUnknownOrDuplicateLegacyBrowserSubfiltersBeforeRead(t *testing.T) {
	t.Parallel()

	for _, values := range [][]sentryv1alpha1.LegacyBrowserFilter{
		{"unsupported"},
		{sentryv1alpha1.LegacyBrowserIE, sentryv1alpha1.LegacyBrowserIE},
	} {
		adapter := &fakeAdapter{}
		result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
			Organization: "company", ProjectSlug: "checkout",
			Desired:   &sentryv1alpha1.ProjectSettings{Filters: &sentryv1alpha1.FilterSettings{LegacyBrowsers: &values}},
			Authorize: func(context.Context) error { t.Fatal("authorization called"); return nil },
		})
		if !errors.Is(result.Err(), ErrInvalidDesiredState) || adapter.reads != 0 {
			t.Fatalf("values=%v result=%#v reads=%d", values, result, adapter.reads)
		}
	}
}

func TestEngineValidatesBeforeReadsOrWrites(t *testing.T) {
	t.Parallel()

	invalid := []string{"authorization", "authorization"}
	adapter := &fakeAdapter{}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      &sentryv1alpha1.ProjectSettings{PII: &sentryv1alpha1.PIISettings{SensitiveFields: &invalid}},
		Authorize:    func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if !errors.Is(result.Err(), ErrInvalidDesiredState) || adapter.reads != 0 || len(adapter.projectUpdates) != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineRemovedBlocksStopManagementWithoutReset(t *testing.T) {
	t.Parallel()

	adapter := &fakeAdapter{}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      nil,
		Authorize:    func(context.Context) error { t.Fatal("authorization called"); return nil },
	})
	if result.Filters.Managed || result.PII.Managed || result.Behavior.Managed || result.Grouping.Managed || result.Symbolication.Managed || result.Err() != nil || adapter.reads != 0 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineContinuesIndependentPIIAfterFilterComponentFailure(t *testing.T) {
	t.Parallel()

	trueValue := true
	falseValue := false
	adapter := &fakeAdapter{
		filters: []Filter{{ID: "localhost", Active: true}},
		state:   ProjectState{ScrubData: &falseValue},
	}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired: &sentryv1alpha1.ProjectSettings{
			Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue},
			PII:     &sentryv1alpha1.PIISettings{ScrubData: &trueValue},
		},
		Authorize: func(context.Context) error { return nil },
	})
	if !errors.Is(result.Filters.Err, ErrUnsupportedFilter) || result.PII.Err != nil || result.PII.Changed != 1 || len(adapter.projectUpdates) != 1 {
		t.Fatalf("result=%#v adapter=%#v", result, adapter)
	}
}

func TestEngineAuthorizationChangePreventsMutation(t *testing.T) {
	t.Parallel()

	trueValue := true
	adapter := &fakeAdapter{filters: []Filter{{ID: "browser-extensions", Active: false}}}
	denied := errors.New("policy changed")
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), Input{
		Organization: "company",
		ProjectSlug:  "checkout",
		Desired:      &sentryv1alpha1.ProjectSettings{Filters: &sentryv1alpha1.FilterSettings{BrowserExtensions: &trueValue}},
		Authorize:    func(context.Context) error { return denied },
	})
	if !errors.Is(result.Filters.Err, denied) || len(adapter.filterUpdates) != 0 {
		t.Fatalf("result=%#v updates=%#v", result, adapter.filterUpdates)
	}
}

type filterUpdate struct {
	id    string
	patch FilterPatch
}

type projectUpdate struct {
	patch ProjectPatch
	etag  string
}

type fakeAdapter struct {
	filters                 []Filter
	state                   ProjectState
	reads                   int
	filterUpdates           []filterUpdate
	projectUpdates          []projectUpdate
	dropGrouping            bool
	dropReleaseAutoCreation bool
}

func (a *fakeAdapter) ListFilters(context.Context, string, string) ([]Filter, error) {
	a.reads++
	return append([]Filter(nil), a.filters...), nil
}

func (a *fakeAdapter) UpdateFilter(_ context.Context, _, _, id string, patch FilterPatch) error {
	a.filterUpdates = append(a.filterUpdates, filterUpdate{id: id, patch: patch})
	return nil
}

func (a *fakeAdapter) GetProjectState(context.Context, string, string) (ProjectState, error) {
	a.reads++
	return a.state, nil
}

func (a *fakeAdapter) UpdateProjectState(_ context.Context, _, _ string, patch ProjectPatch, etag string) error {
	a.projectUpdates = append(a.projectUpdates, projectUpdate{patch: patch, etag: etag})
	if patch.ResolveAge != nil {
		a.state.ResolveAge = patch.ResolveAge
	}
	if patch.DefaultEnvironment != nil {
		a.state.DefaultEnvironment = patch.DefaultEnvironment
	}
	if patch.StoreCrashReports != nil {
		a.state.StoreCrashReports = patch.StoreCrashReports
	}
	if patch.ScrapeJavaScript != nil {
		a.state.ScrapeJavaScript = patch.ScrapeJavaScript
	}
	if patch.AllowedDomains != nil {
		a.state.AllowedDomains = patch.AllowedDomains
	}
	if patch.ReleaseAutoCreation != nil && !a.dropReleaseAutoCreation {
		a.state.ReleaseAutoCreation = patch.ReleaseAutoCreation
	}
	if patch.GroupingEnhancements != nil && !a.dropGrouping {
		a.state.GroupingEnhancements = patch.GroupingEnhancements
	}
	if patch.FingerprintingRules != nil && !a.dropGrouping {
		a.state.FingerprintingRules = patch.FingerprintingRules
	}
	return nil
}

func stringSlicePointer(value []string) *[]string { return &value }

func stringPointer(value string) *string { return &value }

func activeFilterPatch(value bool) FilterPatch { return FilterPatch{Active: &value} }
