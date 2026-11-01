#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/settings ./internal/issueownership ./internal/symbolsource ./internal/sentryapi
KUBEBUILDER_ASSETS="$envtest_assets" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/controller
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/settings ./internal/issueownership ./internal/symbolsource ./internal/sentryapi ./internal/controller

for version in sentry-24.7.1 sentry-26.10.1; do
  jq -e '
    all(.[]; (.id | IN("browser-extensions", "localhost", "web-crawlers")) and (.active | type == "boolean"))
  ' "testdata/http-fixtures/$version/filters-response.json" >/dev/null
  jq -e '. == {"active": true}' "testdata/http-fixtures/$version/filter-update-request.json" >/dev/null
  jq -e '
    (.dataScrubber | type == "boolean") and
    (.dataScrubberDefaults | type == "boolean") and
    (.scrubIPAddresses | type == "boolean") and
    (.sensitiveFields | type == "array") and
    (.allowedDomains | sort) == ["*.checkout.example.com", "https://checkout.example.com:443"]
  ' "testdata/http-fixtures/$version/project-settings-update-request.json" >/dev/null
  jq -e '
    (.relayPiiConfig | fromjson | (.rules | length == 2) and (.applications["$message"] == ["0"])) and
    (.groupingEnhancements | contains("stack.abs_path:")) and
    (.fingerprintingRules | contains("error.type:")) and
    (keys | sort) == ["fingerprintingRules", "groupingEnhancements", "relayPiiConfig"]
  ' "testdata/http-fixtures/$version/project-advanced-settings-update-request.json" >/dev/null
  jq -e '
    . == {"builtinSymbolSources": ["electron", "microsoft", "nuget"]}
  ' "testdata/http-fixtures/$version/project-symbolication-update-request.json" >/dev/null
  jq -e '
    (length == 3) and
    ([.[].type] == ["http", "s3", "gcs"]) and
    (all(.[]; (.id | startswith("sko-fixture-")) and (.name | startswith("sko:")))) and
    (.[0].url == "https://symbols.example.invalid") and
    (.[0].password == "fixture-password-not-a-real-secret") and
    (.[1].secret_key == "fixture-secret-key-not-a-real-secret") and
    (.[2].private_key == "fixture-private-key-not-a-real-secret") and
    (all(.[]; .type != "azure"))
  ' "testdata/http-fixtures/$version/project-custom-symbol-sources.json" >/dev/null
done

if rg -n 'json:"(profile|rawSettings|secondaryGrouping)|settings\.profile|recommended-v1|strict-pii-v1' api/v1alpha1 config/crd/bases testdata/t01/golden; then
  echo 'unsupported grouping profile, secondary migration, or raw settings leaked into the alpha API' >&2
  exit 1
fi

rg -q 'IfMatch:[[:space:]]+etag' internal/settings/http_adapter.go
rg -q 'RetrySafe:[[:space:]]+false' internal/settings/http_adapter.go
rg -q 'FilteredTransactions \*bool' api/v1alpha1/sentryproject_types.go
rg -q 'LegacyBrowsers \*\[\]LegacyBrowserFilter' api/v1alpha1/sentryproject_types.go
rg -q 'TestEngineReconcilesFilteredTransactionsAndLegacyBrowserSubfilters' internal/settings/engine_test.go
rg -q 'TestHTTPAdapterDecodesLegacyBrowserStateAndWritesNarrowFilterPatches' internal/settings/http_adapter_test.go
rg -q 'type BehaviorSettings struct' api/v1alpha1/sentryproject_types.go
rg -q 'AllowedDomains \*\[\]string' api/v1alpha1/sentryproject_types.go internal/settings/http_adapter.go internal/settings/engine.go
rg -q 'TestEngineReconcilesBehaviorSettingsWithOneFreshNarrowPatch' internal/settings/engine_test.go
rg -q 'TestHTTPAdapterRoundTripsBehaviorSettingsAsNarrowConditionalPatch' internal/settings/http_adapter_test.go
rg -q 'type GroupingSettings struct' api/v1alpha1/sentryproject_types.go
rg -q 'AdvancedRules \*\[\]AdvancedPIIRule' api/v1alpha1/sentryproject_types.go
rg -q 'TestEngineSerializesTypedAdvancedPIIAndGroupingRules' internal/settings/engine_test.go
rg -q 'GroupingEnhancements[[:space:]]+\*string' internal/settings/http_adapter.go internal/settings/engine.go
rg -q 'FingerprintingRules[[:space:]]+\*string' internal/settings/http_adapter.go internal/settings/engine.go
rg -q 'type SymbolicationSettings struct' api/v1alpha1/sentryproject_types.go
rg -q 'BuiltinSymbolSources[[:space:]]+\*\[\]string' internal/settings/http_adapter.go internal/settings/engine.go
rg -q 'TestEngineReconcilesPortableBuiltinSymbolSourcesAsASet' internal/settings/engine_test.go
rg -q 'type CustomSymbolSourceSpec struct' api/v1alpha1/sentryproject_types.go
rg -q 'TestEngineOwnsCreateSecretRotationDriftAndExactDelete' internal/symbolsource/engine_test.go
rg -q 'TestHTTPAdapterUsesPublicSymbolSourceCRUDContract' internal/symbolsource/http_adapter_test.go
rg -q 'SymbolSourcesReady' internal/controller/sentryproject_controller.go
rg -q 'type IssueOwnershipSpec struct' api/v1alpha1/sentryproject_types.go
rg -q 'TestEngineReconcilesStructuredTeamRulesAndVerifiesReadback' internal/issueownership/engine_test.go
rg -q 'TestHTTPAdapterReadsNullableRawAndWritesExactPublicContract' internal/issueownership/http_adapter_test.go
rg -q 'RetrySafe:[[:space:]]*false' internal/issueownership/http_adapter.go
rg -q 'IssueOwnershipReady' internal/controller/sentryproject_controller.go
rg -q 'SettingsPartiallyApplied' internal/controller/sentryproject_controller.go

jq -e '
  .versions["24.7.1"].dynamicSamplingBiases == {
    "scope": "project", "read": true, "write": true, "featureGated": true
  } and
  .versions["26.10.1"].dynamicSamplingBiases.write == false and
  .versions["26.10.1"].projectSamplingRates.publishStatus == "private" and
  .versions["26.10.1"].projectSamplingRates.requiresNumericProjectIds == true and
  .versions["26.10.1"].projectSamplingRates.requiresProjectSamplingMode == true and
  .decision.portableProjectQuotaContract == false and
  .decision.managedClientKeyRateLimitAlreadyPublished == true
' testdata/contracts/project-quota-portability.json >/dev/null

if rg -n 'json:"(enableAutoReleaseCreation|raw|user|email)(,|")' api/v1alpha1; then
  echo 'version-specific or unresolved project behavior leaked into the common alpha API' >&2
  exit 1
fi

if rg -n 'json:"(quotaProtection|spikeProtection|dynamicSamplingBiases|targetSampleRate)(,|")' api/v1alpha1; then
  echo 'non-portable quota or dynamic-sampling controls leaked into the common alpha API' >&2
  exit 1
fi

echo 'T05 typed settings, custom symbol-source lifecycle, and team-only issue ownership contracts verified'
