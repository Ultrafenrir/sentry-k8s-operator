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
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" KUBEBUILDER_ASSETS="$product_envtest_assets" \
  "$go_bin" test -race ./internal/audit ./internal/delivery ./internal/telegramdelivery ./internal/httphealth ./internal/sentryhook ./internal/statebackup ./cmd/sentry-hook-producer ./cmd/sentry-hook-receiver
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/audit ./internal/delivery ./internal/telegramdelivery ./internal/sentryhook ./internal/statebackup ./cmd/telegram-delivery-worker ./cmd/sentry-hook-producer ./cmd/sentry-hook-receiver

rg -q 'StateDeadLetter' internal/delivery/queue.go
rg -q 'ResourceVersion' internal/delivery/queue.go
rg -q 'io.LimitReader' internal/telegramdelivery/client.go
rg -q 'FailureRateLimited' internal/telegramdelivery/client.go
rg -q 'FileToken' cmd/telegram-delivery-worker/main.go
rg -q 'TLSClientConfig' cmd/telegram-delivery-worker/main.go
rg -q 'hmac.Equal' internal/sentryhook/handler.go
rg -q 'io.LimitReader' internal/sentryhook/handler.go
rg -q 'SourceBindingKey' internal/sentryhook/handler.go
rg -q 'RenderInput is local operator configuration' internal/sentryhook/render.go
rg -q 'ResolveTelegramDeliveries' internal/sentryhook/producer.go
rg -q 'targetDedupeKey' internal/sentryhook/producer.go
rg -q 'type SourceStore struct' internal/sentryhook/source_store.go
rg -q 'CompareAndSwap' internal/sentryhook/source_store.go
rg -q 'ValidateSourceRecordPayload' internal/sentryhook/source_store.go
rg -q 'type InternalProducerHandler struct' internal/sentryhook/internal.go
rg -q 'tls.RequireAndVerifyClientCert' internal/sentryhook/internal.go
rg -q 'DisallowUnknownFields' internal/sentryhook/internal.go
rg -q 'verifyInternalEnvelope' internal/sentryhook/internal.go
rg -q 'type ProducerClient struct' internal/sentryhook/internal.go
rg -q 'type FileSourceHandler struct' internal/sentryhook/file_source.go
rg -q 'DisallowUnknownFields' internal/sentryhook/verifier_store.go
rg -q 'HMACSecretsBase64' internal/sentryhook/verifier_store.go
rg -q 'PlanVerifierRotation' internal/sentryhook/verifier_store.go
rg -q 'third concurrent key' internal/sentryhook/verifier_store_test.go
rg -q 'type VerifierRotator struct' internal/sentryhook/verifier_rotation.go
rg -q 'SwitchAndVerify' internal/sentryhook/verifier_rotation.go
rg -q 'TestVerifierRotatorResumesSafePartialStates' internal/sentryhook/verifier_rotation_test.go
rg -q 'overlap old secret' internal/sentryhook/file_source_test.go
rg -q 'type Publisher struct' internal/sentryhook/publication.go
rg -q 'Protected routing is' internal/sentryhook/publication.go
rg -q 'verifier-delete,source-delete' internal/sentryhook/publication_test.go
rg -q 'TestPublicationExactCleanupAgainstAPIServer' internal/sentryhook/publication_integration_test.go
rg -q 'DeriveSourceBindingKey' internal/sentryhook/source_identity.go
rg -q 'SourceEndpointURL' internal/sentryhook/source_identity.go
rg -q 'health-address' cmd/sentry-hook-producer/main.go
rg -q 'GetConfigForClient' cmd/sentry-hook-producer/main.go
rg -q 'DialTLSContext' cmd/sentry-hook-receiver/main.go
rg -q -- 'hook-producer-rbac.yaml' config/registration/kustomization.yaml
rg -q -- 'hook-producer.yaml' config/registration/kustomization.yaml
rg -q -- 'hook-producer-network-policy.yaml' config/registration/kustomization.yaml
! sed -n '/name: sentry-hook-producer-queue/,/^---$/p' config/registration/hook-producer-rbac.yaml | rg -q 'update|patch'
rg -q 'port: 6443' config/registration/hook-producer-network-policy.yaml
rg -q 'port: 6443' config/registration/delivery-network-policy.yaml
rg -q 'tls.RequireAndVerifyClientCert' internal/sentryhook/internal.go
rg -q -- 'hook-receiver.yaml' config/registration/kustomization.yaml
rg -q -- 'hook-receiver-network-policy.yaml' config/registration/kustomization.yaml
rg -q 'automountServiceAccountToken: false' config/registration/hook-receiver.yaml
rg -q '/sentry-hook-receiver' config/registration/hook-receiver.yaml
rg -q 'path: /readyz' config/registration/hook-receiver.yaml
! rg -q 'sentry-hook-receiver' config/registration/*rbac.yaml
rg -q 'sentry-hook-receiver-tls' Makefile
rg -q 'sentry-hook-receiver-sources' Makefile
rg -q 'HookSourceNamespace' internal/adminreport/report.go
rg -q 'hook-source-namespace' cmd/kubectl-sentry/main.go
rg -q 'hook-source-namespace.yaml' config/state/kustomization.yaml
rg -q 'Metrics deliberately uses only closed enum labels' internal/telegramdelivery/metrics.go
rg -q 'OldestOutstandingAge' internal/delivery/queue.go
rg -q 'TestQueueBackpressureAgainstAPIServer' internal/delivery/queue_integration_test.go
rg -q 'AdmissionPending' internal/delivery/queue.go
rg -q 'TestPostCreateAPIFailureLeavesUnclaimableResumableAdmission' internal/delivery/queue_test.go
rg -q 'TestCapacityListTimeoutLeavesUnclaimableResumableAdmission' internal/delivery/queue_test.go
rg -q 'TestFinalCreateLostResponseIsIdempotentlyRecovered' internal/delivery/queue_test.go
rg -q 'TestWorkerRedeliversAfterSuccessfulSendAndFailedSettlement' internal/telegramdelivery/worker_test.go
rg -q 'TestAuditFailureBeforeSendBlocksTelegramAndSettlement' internal/telegramdelivery/worker_test.go
rg -q 'TestAuditFailureAfterTelegramSuccessLeavesClaimForAtLeastOnceRecovery' internal/telegramdelivery/worker_test.go
rg -q 'TestDeliveryAuditDoesNotExposeProtectedMessageOrRoute' internal/telegramdelivery/worker_test.go
rg -q 'TestDeliveryManifestContracts' internal/telegramdelivery/manifests_test.go
rg -q 'TestProtectedStateRoundTripAgainstAPIServer' internal/statebackup/backup_integration_test.go
sh -n hack/verify-t08-kind-queue-soak.sh
rg -q 'telegram-delivery-admission' hack/verify-t08-kind-queue-soak.sh
rg -q 't08-soak-event-' hack/verify-t08-kind-queue-soak.sh
sh -n hack/verify-t08-kind-ca-rotation.sh
rg -q 'new-public-ca' hack/verify-t08-kind-ca-rotation.sh
rg -q 'old public trust unexpectedly accepted' hack/verify-t08-kind-ca-rotation.sh
sh -n hack/render-registration-gateway.sh
sh -n hack/manage-registration-gateway.sh
GATEWAY_NAME=public GATEWAY_NAMESPACE=gateway-system GATEWAY_LISTENER=tls \
  TELEGRAM_HOSTNAME=telegram.example.com SENTRY_HOSTNAME=sentry.example.com \
  sh hack/render-registration-gateway.sh >"$tmp_dir/registration-gateway.yaml"
test "$(rg -c '^kind: TLSRoute$' "$tmp_dir/registration-gateway.yaml")" = 2
test "$(rg -c 'apiVersion: gateway.networking.k8s.io/v1' "$tmp_dir/registration-gateway.yaml")" = 2
rg -q 'name: telegram-receiver' "$tmp_dir/registration-gateway.yaml"
rg -q 'name: sentry-hook-receiver' "$tmp_dir/registration-gateway.yaml"
rg -q 'port: 443' "$tmp_dir/registration-gateway.yaml"
rg -q 'port: 8446' "$tmp_dir/registration-gateway.yaml"
! rg -q 'GATEWAY_NAME\|GATEWAY_NAMESPACE\|GATEWAY_LISTENER\|TELEGRAM_HOSTNAME\|SENTRY_HOSTNAME' "$tmp_dir/registration-gateway.yaml"
if GATEWAY_NAME=public GATEWAY_NAMESPACE=gateway-system GATEWAY_LISTENER=tls \
  TELEGRAM_HOSTNAME=shared.example.com SENTRY_HOSTNAME=shared.example.com \
  sh hack/render-registration-gateway.sh >"$tmp_dir/negative.log" 2>&1; then
  echo 'Gateway renderer accepted ambiguous duplicate SNI hostname' >&2
  exit 1
fi
rg -q 'TLS|Passthrough' hack/manage-registration-gateway.sh
rg -q -- '--dry-run=server' hack/manage-registration-gateway.sh
rg -q 'ResolvedRefs' hack/manage-registration-gateway.sh
sh -n hack/verify-t08-kind-gateway-api.sh
rg -q 'gateway-api/releases/download/\$gateway_api_version/standard-install.yaml' hack/verify-t08-kind-gateway-api.sh
rg -q "test \"\$served\" = 'true|true'" hack/verify-t08-kind-gateway-api.sh
rg -q 'refusing to reuse or delete existing kind cluster' hack/verify-t08-kind-gateway-api.sh
sh -n hack/verify-t08-kind-gateway-dataplane.sh
rg -q 'gateway-helm' hack/verify-t08-kind-gateway-dataplane.sh
rg -q 'distroless-v1.39.6' hack/verify-t08-kind-gateway-dataplane.sh
rg -q 'sentry.example.com.*readyz' hack/verify-t08-kind-gateway-dataplane.sh
rg -q 'refusing to reuse or delete existing kind cluster' hack/verify-t08-kind-gateway-dataplane.sh
sh -n hack/verify-t08-kind-monitoring.sh
rg -q 'prometheus-operator/releases/download/\$operator_version/bundle.yaml' hack/verify-t08-kind-monitoring.sh
rg -q 'SentryOperatorTelegramDeliveryMonitoringProbe' hack/verify-t08-kind-monitoring.sh
rg -q 'refusing to reuse or delete existing kind cluster' hack/verify-t08-kind-monitoring.sh

kubectl kustomize config/registration-monitoring >"$tmp_dir/registration-monitoring.yaml"
test "$(rg -c '^kind: ServiceMonitor$' "$tmp_dir/registration-monitoring.yaml")" = 1
test "$(rg -c '^kind: PrometheusRule$' "$tmp_dir/registration-monitoring.yaml")" = 1
rg -q 'port: metrics' "$tmp_dir/registration-monitoring.yaml"
rg -q 'sentry_operator_telegram_delivery_oldest_outstanding_seconds' "$tmp_dir/registration-monitoring.yaml"
rg -q 'outcome="dead_letter"' "$tmp_dir/registration-monitoring.yaml"
rg -q 'sentry_operator_telegram_delivery_metrics_refresh_errors_total' "$tmp_dir/registration-monitoring.yaml"

echo 'T08 signed-hook verifier, signed mTLS internal handoff, protected source resolver, PII-minimal renderer, durable queue, safe Telegram client, isolated worker RBAC and HA workload contracts verified (T08 remains In Progress)'
