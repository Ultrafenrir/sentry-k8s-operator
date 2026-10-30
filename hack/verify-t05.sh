#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/settings ./internal/sentryapi
KUBEBUILDER_ASSETS="$envtest_assets" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/controller
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/settings ./internal/sentryapi ./internal/controller

for version in sentry-24.7.1 sentry-26.10.1; do
  jq -e '
    all(.[]; (.id | IN("browser-extensions", "localhost", "web-crawlers")) and (.active | type == "boolean"))
  ' "testdata/http-fixtures/$version/filters-response.json" >/dev/null
  jq -e '. == {"active": true}' "testdata/http-fixtures/$version/filter-update-request.json" >/dev/null
  jq -e '
    (.dataScrubber | type == "boolean") and
    (.dataScrubberDefaults | type == "boolean") and
    (.scrubIPAddresses | type == "boolean") and
    (.sensitiveFields | type == "array")
  ' "testdata/http-fixtures/$version/project-settings-update-request.json" >/dev/null
done

if rg -n 'fingerprintingRules|fingerprintRules|settings\.profile|recommended-v1|strict-pii-v1|json:"grouping|json:"advanced' api/v1alpha1 config/crd/bases testdata/t01/golden; then
  echo 'unsupported profile/grouping/advanced leaked into the alpha API' >&2
  exit 1
fi

rg -q 'IfMatch:[[:space:]]+etag' internal/settings/http_adapter.go
rg -q 'RetrySafe:[[:space:]]+false' internal/settings/http_adapter.go
rg -q 'SettingsPartiallyApplied' internal/controller/sentryproject_controller.go

echo 'T05 typed filters/PII, fresh merge, conditional update and partial component contracts verified'
