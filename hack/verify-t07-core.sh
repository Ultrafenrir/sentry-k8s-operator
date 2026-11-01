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
rg -q 'TestLegacyHTTPAdapterRendersUserCountAggregate' internal/alerts/legacy_http_adapter_test.go
rg -q 'LegacyCombinedAdapter' internal/alerts/router.go
rg -q 'TestLegacyRouterSupportsMixedIssueAndMetricIdentities' internal/alerts/legacy_http_adapter_test.go
rg -q 'reconcileAlerts' internal/controller/sentryproject_controller.go
rg -q 'AlertsReady' internal/controller/sentryproject_controller.go
rg -q 'AlertCompatibilityLegacy24' api/v1alpha1/sentryconnection_types.go
rg -q 'Alerts \*map\[string\]AlertSpec' api/v1alpha1/sentryproject_types.go
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

rg -q 'json:"alerts,omitempty"' api/v1alpha1/sentryproject_types.go
rg -q 'legacy-24.7.1' config/crd/bases/sentryconfiguration.io_sentryconnections.yaml
rg -q 'event-count' config/crd/bases/sentryconfiguration.io_sentryprojects.yaml
rg -q 'user-count' config/crd/bases/sentryconfiguration.io_sentryprojects.yaml
rg -Fq 'count_unique(user)' internal/alerts/legacy_http_adapter_test.go
rg -Fq 'count_unique(user)' internal/alerts/modern_http_adapter_test.go

echo 'T07 typed API, protected controller lifecycle and modern/legacy HTTP adapters verified; live action delivery remains gated'
