#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
if [ ! -x "$go_bin" ]; then
  go_bin=$(command -v go)
fi
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/sentryapi -run 'TestClient(ClassifiesStatusesWithoutResponseBody|RetriesOnlyExplicitlySafeRequests|RejectsUnsafeOriginsAndRedirects)|TestTimedOutPOSTIsNotBlindlyRetried|TestResponseBodyTimeoutKeepsTimeoutClassification|TestDecodeErrorIsRedacted' -count=1
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/project -run 'TestLostPOSTResponseIsRecoveredByMarkerWithoutSecondPOST|TestSubmittedCreateWithoutRecoverableObjectDoesNotRetry|TestCompetingWritersAcrossAliasesGetOneClaimAndOnePOST' -count=1
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/alerts -run 'TestSubmittedEnsureRecoversLostResponseWithoutSecondChild|TestModernHTTPAdapterRecoversLostDetectorCreateResponse|TestModernHTTPAdapterFullMetricIssueLifecycle|TestLegacyHTTPAdapterIssueLifecycleAndLostResponseRecovery' -count=1
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/settings -run 'TestEngineContinuesIndependentPIIAfterFilterComponentFailure|TestEngineAuthorizationChangePreventsMutation' -count=1
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/dsn -run 'TestPublisherReauthorizesEveryWriteAndPreservesWorkingSecretOnDenial|TestPublisherTwoPhaseCutoverWaitsForExactConfirmation' -count=1

rg -q 'Retry-After' internal/sentryapi/client_test.go
rg -q 'mutating POST was retried' internal/sentryapi/client_test.go
rg -q 'lost response recovery made' internal/project/engine_test.go
rg -q 'loseDetectorResponseOnce' internal/alerts/modern_http_adapter_test.go
rg -q 'loseIssueResponseOnce' internal/alerts/legacy_http_adapter_test.go
rg -q 'observed\.Marker\.Matches\(marker\)' internal/alerts/modern_http_adapter.go
rg -q 'partial or missing create capabilities were accepted' internal/project/engine_test.go
rg -q 'preservation of a working Secret' docs/t10-packaging-progress.txt

echo 'T10 deterministic failure injection verified with httptest/fakes; live Sentry acceptance remains required'
