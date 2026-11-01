#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
product_envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race -run 'TestJitterDuration|TestProjectController(Recovers|Revokes|Fails|Deletes|Destination)' ./internal/controller
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" KUBEBUILDER_ASSETS="$product_envtest_assets" \
  "$go_bin" test -race -run TestControllers ./internal/controller -ginkgo.focus='bounded connection thundering herd'
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" KUBEBUILDER_ASSETS="$product_envtest_assets" \
  "$go_bin" test -race ./internal/statebackup
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race -run 'TestChallengeJanitor|TestChallengeJitter|TestRouteMigrator|TestRouteStoreMigrat' ./internal/registration
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/adminreport ./cmd/kubectl-sentry
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/audit ./internal/alerts ./internal/project ./internal/registration ./internal/telegramdelivery

rg -q 'NeedLeaderElection.*true' internal/registration/challenge_janitor.go
rg -q 'NeedLeaderElection.*true' internal/registration/route_migrator.go
rg -q -- '--challenge-gc-interval=5m' config/manager/manager.yaml
rg -q 'case "admin-report"' cmd/kubectl-sentry/main.go
rg -q 'case "audit-export"' cmd/kubectl-sentry/main.go
rg -q 'audit.KubernetesStore' cmd/kubectl-sentry/main.go
rg -q 'NeedLeaderElection.*true' internal/audit/retention.go
rg -q 'Immutable:.*immutable' internal/audit/kubernetes.go
rg -q 'TestExportAuditFailsClosedWithoutPartialOutput' cmd/kubectl-sentry/main_test.go
rg -q 'kind: ValidatingAdmissionPolicy' config/state/audit-admission.yaml
rg -q 'operations:' config/state/audit-admission.yaml
rg -q -- '- UPDATE' config/state/audit-admission.yaml
rg -q 'validationActions:' config/state/audit-admission.yaml
rg -q -- '- Deny' config/state/audit-admission.yaml
rg -q -- '--audit-retention=720h' config/manager/manager.yaml
rg -q -- '--audit-namespace=sentry-kubernetes-operator-audit' config/registration/deployment.yaml
rg -q -- '--audit-namespace=sentry-kubernetes-operator-audit' config/registration/delivery-worker.yaml
rg -q 'SHOULD-NOT-LEAK' internal/adminreport/report_test.go
rg -q 'ErrAuditUnavailable' internal/alerts/journal.go
rg -q 'TestAuditFailureBeforeApplyBlocksExternalMutation' internal/alerts/engine_test.go
rg -q 'TestEventHasClosedIdentityFreeShape' internal/audit/audit_test.go
rg -q 'TestAuditFailureBeforeSendBlocksTelegramAndSettlement' internal/telegramdelivery/worker_test.go
rg -q 'TestAuditFailureBeforeCreateBlocksPOSTAndKeepsSubmittedIntent' internal/project/engine_test.go
rg -q 'TestAuditFailureAfterRoutePublishKeepsResumablePendingState' internal/registration/telegram_test.go
rg -q 'TestProjectControllerDestinationCleanupAuditFailsClosedAndRecoversDeletedRoute' internal/controller/registration_recovery_test.go
rg -q 'destinationAudit := &audit.KubernetesStore.*destination-mutation-audit' cmd/main.go
rg -q 'DestinationAudit:.*destinationAudit' cmd/main.go
rg -q 'TestExportFailsClosedOnSemanticallyInvalidStateRecord' internal/statebackup/backup_test.go
rg -q 'TestRestoreRejectsChecksumValidSemanticCorruptionBeforeWrite' internal/statebackup/backup_test.go
rg -q 'TestExportFailsClosedOnInvalidChallengeRouteHookSourceAndDeliveryPayloads' internal/statebackup/backup_test.go
sh -n hack/verify-t09-kind-backup-restore.sh
rg -q 'refusing to reuse existing namespace' hack/verify-t09-kind-backup-restore.sh
rg -q 'USE_EXISTING_CLUSTER=true' hack/verify-t09-kind-backup-restore.sh
rg -q 'statebackup-kind-drill' internal/statebackup/backup_integration_test.go
rg -q 'sentry_operator_condition_transitions_total' internal/controller/metrics.go
rg -q 'sentry_operator_sync_resources' internal/controller/metrics.go
rg -q 'Metrics accepts only closed enum labels' internal/controller/metrics.go
sh -n hack/verify-t09-kind-multinode-failover.sh
rg -q 'unique | length' hack/verify-t09-kind-multinode-failover.sh
rg -q 'SecretInputInvalid' hack/verify-t09-kind-multinode-failover.sh
rg -q 'docker stop.*node_loss_node' hack/verify-t09-kind-multinode-failover.sh
rg -q 'node_loss_holder' hack/verify-t09-kind-multinode-failover.sh
rg -q 't09-node-loss-admission' hack/verify-t09-kind-multinode-failover.sh
rg -q 'docker start.*node_loss_node' hack/verify-t09-kind-multinode-failover.sh
rg -q -- '--sbom=true --provenance=mode=max --metadata-file' Makefile
rg -q 'deploy-production: verify-signed-image' Makefile
rg -q 'deploy-registration-production: verify-signed-image' Makefile
rg -q 'COSIGN_CERTIFICATE_IDENTITY' hack/verify-signed-image.sh
rg -q -- '--certificate-oidc-issuer' hack/verify-signed-image.sh
rg -q 'ARG BASE_IMAGE=.*@sha256:[0-9a-f]{64}' Dockerfile
rg -q 'ARG RUNTIME_IMAGE=.*@sha256:[0-9a-f]{64}' Dockerfile

"$root_dir/.tools/go-1.26.10/bin/go" version >/dev/null
make -s verify-release-tag IMG=registry.example/operator:v1.2.3
make -s verify-base-image-digests
if make -s verify-base-image-digests BASE_IMAGE=docker.io/library/golang:1.26.10 >/dev/null 2>&1; then
  echo 'mutable release builder image unexpectedly accepted' >&2
  exit 1
fi
if make -s verify-base-image-digests RUNTIME_IMAGE=gcr.io/distroless/static:nonroot >/dev/null 2>&1; then
  echo 'mutable release runtime image unexpectedly accepted' >&2
  exit 1
fi
if make -s verify-release-tag IMG=registry.example/operator:latest >/dev/null 2>&1; then
  echo 'latest release tag unexpectedly accepted' >&2
  exit 1
fi
make -s verify-image-digest IMG="registry.example/operator@sha256:$(printf 'a%.0s' $(seq 1 64))"
if make -s verify-image-digest IMG=registry.example/operator:v1.2.3 >/dev/null 2>&1; then
  echo 'mutable production image unexpectedly accepted' >&2
  exit 1
fi
make -s verify-signature-policy
make -s verify-release-workflow
printf '{"containerimage.digest":"sha256:%s"}\n' "$(printf 'b%.0s' $(seq 1 64))" >"$tmp_dir/build-metadata.json"
make -s verify-release-metadata RELEASE_METADATA="$tmp_dir/build-metadata.json"
make -s verify-helm
make -s verify-samples
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -run TestDeploymentManifestContracts ./internal/controller

kubectl kustomize config/default >"$tmp_dir/default.yaml"
rg -q 'name: sentry-kubernetes-operator-controller-manager' "$tmp_dir/default.yaml"
rg -q 'name: sentry-kubernetes-operator-allow-webhook-traffic' "$tmp_dir/default.yaml"
rg -q 'name: sentry-kubernetes-operator-allow-metrics-traffic' "$tmp_dir/default.yaml"

kubectl kustomize config/controller-monitoring >"$tmp_dir/controller-monitoring.yaml"
rg -q 'sentry_operator_reconciles_total' "$tmp_dir/controller-monitoring.yaml"
rg -q 'sentry_operator_condition_transitions_total' "$tmp_dir/controller-monitoring.yaml"
rg -q 'sentry_operator_sync_resources' "$tmp_dir/controller-monitoring.yaml"
rg -q 'sentry-kubernetes-operator-controller-manager-metrics-service.sentry-kubernetes-operator-system.svc.cluster.local' "$tmp_dir/controller-monitoring.yaml"
rg -q 'insecureSkipVerify: false' "$tmp_dir/controller-monitoring.yaml"
! rg -q 'insecureSkipVerify: true' "$tmp_dir/controller-monitoring.yaml"

sh -n hack/render-controller-monitoring-access.sh
sh -n hack/manage-controller-monitoring-access.sh
sh -n hack/verify-t09-kind-monitoring-access.sh
sh -n hack/verify-t09-kind-controller-monitoring.sh
PROMETHEUS_SERVICE_ACCOUNT_NAME=prometheus PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE=monitoring \
  sh hack/render-controller-monitoring-access.sh >"$tmp_dir/controller-monitoring-access.yaml"
rg -q 'name: sentry-kubernetes-operator-controller-metrics-reader' "$tmp_dir/controller-monitoring-access.yaml"
rg -q 'name: sentry-kubernetes-operator-metrics-reader' "$tmp_dir/controller-monitoring-access.yaml"
rg -q 'name: prometheus' "$tmp_dir/controller-monitoring-access.yaml"
rg -q 'namespace: monitoring' "$tmp_dir/controller-monitoring-access.yaml"
! rg -q 'PROMETHEUS_SERVICE_ACCOUNT_NAME\|PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE' "$tmp_dir/controller-monitoring-access.yaml"
if PROMETHEUS_SERVICE_ACCOUNT_NAME=prometheus PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE=invalid.namespace \
  sh hack/render-controller-monitoring-access.sh >"$tmp_dir/negative.log" 2>&1; then
  echo 'controller monitoring access renderer accepted invalid namespace' >&2
  exit 1
fi
rg -q 'refusing to replace existing ClusterRoleBinding' hack/verify-t09-kind-monitoring-access.sh
rg -q 'auth can-i get /metrics' hack/verify-t09-kind-monitoring-access.sh
rg -q 'auth can-i get secrets' hack/verify-t09-kind-monitoring-access.sh
rg -q 'refusing to reuse or delete existing kind cluster' hack/verify-t09-kind-controller-monitoring.sh
rg -q 'lastError.*contains.*403' hack/verify-t09-kind-controller-monitoring.sh
rg -q 'manage-controller-monitoring-access.sh.*deploy' hack/verify-t09-kind-controller-monitoring.sh
rg -q 'sentry_operator_reconciles_total' hack/verify-t09-kind-controller-monitoring.sh
rg -q 'SentryOperatorReconcileErrors SentryOperatorConditionFailureBurst SentryOperatorSynchronizationBlocked' hack/verify-t09-kind-controller-monitoring.sh

kubectl kustomize config/prometheus >"$tmp_dir/legacy-monitoring.yaml"
rg -q 'insecureSkipVerify: false' "$tmp_dir/legacy-monitoring.yaml"
rg -q 'sentry-kubernetes-operator-controller-manager-metrics-service.sentry-kubernetes-operator-system.svc.cluster.local' "$tmp_dir/legacy-monitoring.yaml"
rg -q 'name: metrics-server-cert' "$tmp_dir/legacy-monitoring.yaml"
rg -q 'key: ca.crt' "$tmp_dir/legacy-monitoring.yaml"
! rg -q 'insecureSkipVerify: true' "$tmp_dir/legacy-monitoring.yaml"

echo 'T09 bounded HA, recovery, durable audit/export/retention, redacted admin report, attested-build/digest gates, PDB and NetworkPolicy contracts verified; production evidence remains deferred'
