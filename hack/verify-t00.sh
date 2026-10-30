#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture_root="$root_dir/testdata/http-fixtures"
legacy_fixture_dir="$fixture_root/sentry-24.7.1"
modern_fixture_dir="$fixture_root/sentry-26.10.1"
alpha_surface="$root_dir/testdata/contracts/t00-alpha-surface.json"
modern_image="$root_dir/testdata/contracts/sentry-26.10.1-image.json"
alert_catalog="$root_dir/testdata/contracts/t00-alert-catalog.json"
kind_images="$root_dir/testdata/contracts/t00-kind-images.json"

find "$fixture_root" -type f -name '*.json' -print | while IFS= read -r fixture; do
  jq empty "$fixture"
done

jq -e '.default_rules == false and (.platform | not)' \
  "$legacy_fixture_dir/project-create-request.json" >/dev/null
jq -e '(.name | length) <= 50 and (.name | endswith("[managed;o=payments;m=company-sentry-op]"))' \
  "$legacy_fixture_dir/project-create-request.json" >/dev/null
jq -e '.[0].isActive == true and .[0].label == "Default" and .[0].dsn.public' \
  "$legacy_fixture_dir/client-keys-response.json" >/dev/null
jq -e 'map(.id) == ["browser-extensions", "localhost", "web-crawlers"]' \
  "$legacy_fixture_dir/filters-response.json" >/dev/null
jq -e '.events == ["event.alert"] and (.url | startswith("https://"))' \
  "$legacy_fixture_dir/service-hook-create-request.json" >/dev/null
jq -e '
  (.project | keys | sort) == ["name", "slug"] and
  (.group | keys | sort) == ["id", "title", "url"] and
  (.event | keys | sort) == ["eventID", "message", "url"]
' "$legacy_fixture_dir/service-hook-event-alert.body.json" >/dev/null

jq -e 'keys == ["enabled"] and .enabled == false' \
  "$modern_fixture_dir/workflow-disable-request.json" >/dev/null
jq -e 'length == 1 and .[0].id == "__WORKFLOW_ID__" and .[0].enabled == false' \
  "$modern_fixture_dir/workflow-disable-response.json" >/dev/null
jq -e '
  .events == ["event.alert"] and .version == 0 and .isActive == true and
  (.url | startswith("https://"))
' "$modern_fixture_dir/service-hook-create-request.json" >/dev/null
jq -e '
  .default_rules == false and (.platform | not) and
  (.name | length) <= 50 and
  (.name | endswith("[managed;o=payments;m=company-sentry-op]"))
' "$modern_fixture_dir/project-create-request.json" >/dev/null
jq -e '
  .slug == "checkout" and .platform == null and .status == "active" and
  (.name | endswith("[managed;o=payments;m=company-sentry-op]"))
' "$modern_fixture_dir/project-create-response.json" >/dev/null
jq -e '
  length == 1 and .[0].isActive == true and .[0].label == "Default" and
  .[0].name == "Default" and .[0].dsn.public
' "$modern_fixture_dir/client-keys-response.json" >/dev/null
jq -e '
  .dataScrubber == true and .dataScrubberDefaults == true and
  .scrubIPAddresses == true and
  .sensitiveFields == ["authorization", "password"] and
  .safeFields == ["request.headers.x-request-id"]
' "$modern_fixture_dir/project-settings-update-request.json" >/dev/null
jq -e '.active == true' "$modern_fixture_dir/filter-update-request.json" >/dev/null
jq -e 'map(.id) == ["browser-extensions", "localhost", "web-crawlers"]' \
  "$modern_fixture_dir/filters-response.json" >/dev/null

jq -e '
  .settings.profile == {"published": false, "names": []} and
  .settings.filters == ["browserExtensions", "localhost", "webCrawlers"] and
  .settings.pii == ["safeFields", "scrubData", "scrubDefaults", "scrubIPAddresses", "sensitiveFields"] and
  .settings.grouping == {"published": false, "fields": []} and
  .settings.advanced == {"published": false, "fields": []} and
  .alerts.published == false and
  .alerts.presets == [] and
  .alerts.commonFields == [] and
  .alerts.advanced == {"published": false, "fields": []}
' "$alpha_surface" >/dev/null
jq -e '
  .image == "ghcr.io/getsentry/sentry:26.10.1" and
  .mediaType == "application/vnd.oci.image.index.v1+json" and
  (.indexDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.platforms["linux/amd64"] | test("^sha256:[0-9a-f]{64}$")) and
  (.platforms["linux/arm64"] | test("^sha256:[0-9a-f]{64}$"))
' "$modern_image" >/dev/null
jq -e '
  .publishedInAlpha == false and
  (.legacyIssue.conditions | length) == 12 and
  (.legacyIssue.filters | length) == 9 and
  (.legacyIssue.baseActions | length) == 4 and
  .legacyIssue.targetDeliveryActions.slack == "sentry.integrations.slack.notify_action.SlackNotifyServiceAction" and
  .legacyIssue.targetDeliveryActions.pagerduty == "sentry.integrations.pagerduty.notify_action.PagerDutyNotifyServiceAction" and
  .legacyIssue.frequencyIntervals.values == ["1m", "5m", "15m", "1h", "1d", "1w", "30d"] and
  .legacyMetric.errorCountMapping == {"queryType": 0, "dataset": "events", "aggregate": "count()"} and
  .legacyMetric.timeWindow == {"unit": "minutes", "minimum": 1, "maximum": 1440} and
  .legacyMetric.crashRateTimeWindows.values == [30, 60, 120, 240, 720, 1440] and
  .modern.status == "source-catalogued-runtime-availability-unknown" and
  (.modern.conditionEnum | length) == 38 and
  (.modern.actionEnum | length) == 16 and
  .modern.legacyConditionsHiddenByDiscoveryEndpoint == [
    "event_created_by_detector", "event_seen_count", "new_high_priority_issue",
    "existing_high_priority_issue", "issue_resolution_change", "issue_priority_equals"
  ] and
  .modern.enumWithoutSourceRegistration == ["issue_open_duration"] and
  .modern.actionsExcludedByDiscoveryEndpoint == {
    "deprecated": ["plugin"], "provisionedSeparately": ["seer_workflow"]
  } and
  .modern.targetDeliveryActions == ["slack", "pagerduty"] and
  .modern.dataSourceRegistrations == [
    "snuba_query_subscription", "uptime_subscription", "cron_monitor"
  ] and
  .modern.releasedDetectorTypesWithHandler == [
    "error", "metric_issue", "uptime_domain_failure"
  ] and
  .modern.featureGatedDetectorTypesWithHandler == [
    "sourcemap_configuration", "preprod_size_analysis"
  ] and
  .modern.detectorTypesWithoutHandler == ["monitor_check_in_failure", "issue_stream"]
' "$alert_catalog" >/dev/null
jq -e '
  .builder.kindVersion == "v0.32.0" and
  .builder.platform == "darwin/arm64" and
  (.builder.baseImageDigest | test("^sha256:[0-9a-f]{64}$")) and
  (.images | keys) == ["v1.35.9", "v1.36.5"] and
  ([.images[] | .result] | all(. == "Passed")) and
  ([.images[] | .platform] | all(. == "linux/arm64")) and
  ([.images[] | .localImageId] | all(test("^sha256:[0-9a-f]{64}$"))) and
  ([.images[] | .serverGitCommit] | all(test("^[0-9a-f]{40}$"))) and
  .checks == [
    "node-system-health", "structural-crd", "openapi-defaults", "cel",
    "tls-admission-fail-closed", "status-subresource", "probe-rbac-separation",
    "pdb-eviction", "default-deny-network-policy", "conversion-none",
    "lease-leader-failover"
  ]
' "$kind_images" >/dev/null

for version_dir in "$legacy_fixture_dir" "$modern_fixture_dir"; do
  body="$version_dir/service-hook-event-alert.body.json"
  actual_signature=$(openssl dgst -sha256 -hmac 'fixture-only-not-a-secret' "$body" | awk '{print $NF}')
  test "$actual_signature" = '501d899843827d99c020a0ff528d85e5929c9afb5331952a8d0a58c7b0246e1a'
  fixture_signature=$(jq -r '.["X-ServiceHook-Signature"]' \
    "$version_dir/service-hook-event-alert.headers.json")
  test "$fixture_signature" = "$actual_signature"
done

if rg -n '(Authorization: Bearer [A-Za-z0-9]|https://[^/@]+:[^/@]+@|sentry\.io/[0-9]+)' \
  "$fixture_root"; then
  echo 'possible credential or real DSN found in fixtures' >&2
  exit 1
fi

echo 'T00 source-derived fixtures verified'
