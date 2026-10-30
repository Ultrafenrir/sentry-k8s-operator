#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/audit ./internal/alerts ./internal/binding
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/audit ./internal/alerts ./internal/binding

rg -q 'type AlertBinding struct' internal/binding/types.go
rg -q 'validateAlertBindings' internal/binding/store.go
rg -q 'PinnedAdapter' internal/alerts/engine.go
rg -q 'input.Authorize' internal/alerts/engine.go
rg -q 'ErrUnsupportedUpdate' internal/alerts/engine.go
rg -q 'result.Tombstones\[key\] = true' internal/alerts/engine.go
rg -q 'removedKeys' internal/alerts/engine.go
rg -q 'ReleasableDestinations' internal/alerts/engine.go
rg -q 'OperationSubmitted' internal/alerts/journal.go
rg -q 'finishMutation' internal/alerts/journal.go
rg -q 'ErrAuditUnavailable' internal/alerts/journal.go
rg -q 'ModernMetricIssueAdapter' internal/alerts/modern_http_adapter.go
rg -q 'observed\.Marker\.Matches\(marker\)' internal/alerts/modern_http_adapter.go
rg -q 'TestModernHTTPAdapterFullMetricIssueLifecycle' internal/alerts/modern_http_adapter_test.go
rg -q 'TestModernHTTPAdapterRecoversLostDetectorCreateResponse' internal/alerts/modern_http_adapter_test.go
rg -q 'LegacyIssueAdapter' internal/alerts/legacy_http_adapter.go
rg -q 'LegacyMetricAdapter' internal/alerts/legacy_http_adapter.go
rg -q 'ErrLegacyToggleUnsupported' internal/alerts/legacy_http_adapter.go
rg -q 'TestLegacyHTTPAdapterIssueLifecycleAndLostResponseRecovery' internal/alerts/legacy_http_adapter_test.go
rg -q 'TestLegacyHTTPAdapterMetricLifecycleAndPayload' internal/alerts/legacy_http_adapter_test.go
rg -q 'ReasonJournalSubmitted' internal/audit/audit.go
rg -q 'ReasonResultRecovered' internal/audit/audit.go
rg -q 'TestAbsentAlertsDoesNotManageExistingChildren' internal/alerts/engine_test.go
rg -q 'TestLifecyclePinsAdapterOrdersDeleteBeforeCreateAndTombstones' internal/alerts/engine_test.go
rg -q 'TestUpdateAndToggleReauthorizeEveryMutation' internal/alerts/engine_test.go
rg -q 'TestSubmittedEnsureRecoversLostResponseWithoutSecondChild' internal/alerts/engine_test.go
rg -q 'TestSubmittedDifferentIntentFailsClosed' internal/alerts/engine_test.go
rg -q 'TestAuditFailureBeforeApplyBlocksExternalMutation' internal/alerts/engine_test.go
rg -q 'TestSubmittedVerifiedResultRecordsRecoveryWithoutReapply' internal/alerts/engine_test.go
rg -q 'TestEventHasClosedIdentityFreeShape' internal/audit/audit_test.go
rg -q 'TestFailedAlertDeleteBlocksDestinationCleanup' internal/alerts/engine_test.go
rg -q 'TestBindingValidationRejectsCorruptProtectedAlertState' internal/binding/store_test.go
rg -q 'TestOperationJournalRejectsUnsafeTransitions' internal/binding/store_test.go

if rg -n 'json:"alerts' api/v1alpha1 config/crd/bases; then
  echo 'unaccepted alerts surface leaked into the public alpha API' >&2
  exit 1
fi

echo 'T07 protected lifecycle and modern/legacy HTTP adapters verified; controller wiring and live action delivery remain gated'
