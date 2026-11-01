#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
chart_dir="$root_dir/charts/sentry-kubernetes-operator"
helm_bin=${HELM:-helm}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

cp -R "$chart_dir" "$tmp_dir/chart"
CHART_DIR="$tmp_dir/chart" "$root_dir/hack/update-helm-chart.sh"
diff -ru "$chart_dir/files" "$tmp_dir/chart/files"
diff -ru "$chart_dir/crds" "$tmp_dir/chart/crds"
cmp "$root_dir/LICENSE" "$chart_dir/LICENSE"
cmp "$root_dir/NOTICE" "$chart_dir/NOTICE"

cp "$root_dir/config/manager/kustomization.yaml" "$tmp_dir/manager-kustomization.yaml"
cp "$root_dir/config/registration/kustomization.yaml" "$tmp_dir/registration-kustomization.yaml"
KUSTOMIZE_BIN=${KUSTOMIZE:-"$root_dir/bin/kustomize"} \
  "$root_dir/hack/render-kustomize-image.sh" default registry.example/operator:verify >"$tmp_dir/render-default.yaml"
KUSTOMIZE_BIN=${KUSTOMIZE:-"$root_dir/bin/kustomize"} \
  "$root_dir/hack/render-kustomize-image.sh" registration registry.example/operator:verify >"$tmp_dir/render-registration.yaml"
cmp "$root_dir/config/manager/kustomization.yaml" "$tmp_dir/manager-kustomization.yaml"
cmp "$root_dir/config/registration/kustomization.yaml" "$tmp_dir/registration-kustomization.yaml"
test "$(rg -c 'image: registry.example/operator:verify' "$tmp_dir/render-default.yaml")" = 1
test "$(rg -c 'image: registry.example/operator:verify' "$tmp_dir/render-registration.yaml")" = 5

"$helm_bin" lint "$chart_dir" --kube-version 1.35.0 >/dev/null
"$helm_bin" lint "$chart_dir" --kube-version 1.36.0 >/dev/null
"$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 --include-crds >"$tmp_dir/default.yaml"
"$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.36.0 --include-crds >"$tmp_dir/upper.yaml"
"$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set registration.enabled=true \
  --set registrationGateway.enabled=true \
  --set registrationGateway.gatewayName=public \
  --set registrationGateway.gatewayNamespace=gateway-system \
  --set registrationGateway.listenerName=tls \
  --set registrationGateway.telegramHostname=telegram.example.com \
  --set registrationGateway.sentryHostname=sentry.example.com \
  --set registrationMonitoring.enabled=true \
  --set controllerMonitoring.enabled=true \
  --set controllerMonitoring.access.enabled=true \
  --set controllerMonitoring.access.serviceAccountName=prometheus \
  --set controllerMonitoring.access.serviceAccountNamespace=monitoring >"$tmp_dir/all.yaml"
"$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set production=true \
  --set image.repository=registry.example/sentry-operator \
  --set image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >"$tmp_dir/production.yaml"

test "$(rg -c '^kind: CustomResourceDefinition$' "$tmp_dir/default.yaml")" = 3
test "$(rg -c 'helm.sh/resource-policy: keep' "$tmp_dir/default.yaml")" = 5
! rg -q 'controller:latest' "$tmp_dir/default.yaml"
! rg -q 'app.kubernetes.io/managed-by: kustomize' "$tmp_dir/default.yaml"
rg -q 'app.kubernetes.io/managed-by: Helm' "$tmp_dir/default.yaml"
rg -q -- '--metrics-cert-path=/tmp/k8s-metrics-server/metrics-certs' "$tmp_dir/default.yaml"
rg -q 'secretName: metrics-server-cert' "$tmp_dir/default.yaml"
rg -q 'sentry-kubernetes-operator-controller-manager-metrics-service.sentry-kubernetes-operator-system.svc.cluster.local' "$tmp_dir/default.yaml"
! rg -q 'SERVICE_NAME|SERVICE_NAMESPACE' "$tmp_dir/default.yaml"
! rg -q 'telegram-delivery-worker' "$tmp_dir/default.yaml"
! rg -q '^kind: TLSRoute$' "$tmp_dir/default.yaml"
! rg -q 'name: sentry-kubernetes-operator-controller-metrics-reader' "$tmp_dir/default.yaml"
rg -q 'telegram-delivery-worker' "$tmp_dir/all.yaml"
rg -q 'sentry-hook-producer' "$tmp_dir/all.yaml"
rg -q 'sentry-hook-receiver' "$tmp_dir/all.yaml"
test "$(rg -c '^kind: TLSRoute$' "$tmp_dir/all.yaml")" = 2
rg -q 'apiVersion: gateway.networking.k8s.io/v1' "$tmp_dir/all.yaml"
rg -q 'name: public' "$tmp_dir/all.yaml"
rg -q 'namespace: gateway-system' "$tmp_dir/all.yaml"
rg -q 'sectionName: tls' "$tmp_dir/all.yaml"
rg -q -- '- telegram.example.com' "$tmp_dir/all.yaml"
rg -q -- '- sentry.example.com' "$tmp_dir/all.yaml"
! rg -q 'GATEWAY_NAME\|GATEWAY_NAMESPACE\|GATEWAY_LISTENER\|TELEGRAM_HOSTNAME\|SENTRY_HOSTNAME' "$tmp_dir/all.yaml"
test "$(rg -c '^kind: ServiceMonitor$' "$tmp_dir/all.yaml")" = 2
test "$(rg -c '^kind: PrometheusRule$' "$tmp_dir/all.yaml")" = 2
rg -q 'sentry_operator_reconciles_total' "$tmp_dir/all.yaml"
rg -q 'sentry_operator_condition_transitions_total' "$tmp_dir/all.yaml"
rg -q 'sentry_operator_sync_resources' "$tmp_dir/all.yaml"
rg -q 'insecureSkipVerify: false' "$tmp_dir/all.yaml"
! rg -q 'insecureSkipVerify: true' "$tmp_dir/all.yaml"
rg -q 'name: sentry-kubernetes-operator-controller-metrics-reader' "$tmp_dir/all.yaml"
rg -q 'name: sentry-kubernetes-operator-metrics-reader' "$tmp_dir/all.yaml"
rg -q 'name: prometheus' "$tmp_dir/all.yaml"
rg -q 'namespace: monitoring' "$tmp_dir/all.yaml"
! rg -q 'PROMETHEUS_SERVICE_ACCOUNT_NAME\|PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE' "$tmp_dir/all.yaml"
rg -q 'image: registry.example/sentry-operator@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$tmp_dir/production.yaml"

if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 --set production=true >"$tmp_dir/negative.log" 2>&1; then
  echo 'production chart accepted a mutable image' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 --set registrationMonitoring.enabled=true >"$tmp_dir/negative.log" 2>&1; then
  echo 'monitoring chart accepted disabled registration workload' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set controllerMonitoring.access.enabled=true \
  --set controllerMonitoring.access.serviceAccountName=prometheus \
  --set controllerMonitoring.access.serviceAccountNamespace=monitoring >"$tmp_dir/negative.log" 2>&1; then
  echo 'controller metrics access accepted disabled controller monitoring' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set controllerMonitoring.enabled=true \
  --set controllerMonitoring.access.enabled=true >"$tmp_dir/negative.log" 2>&1; then
  echo 'controller metrics access accepted missing Prometheus ServiceAccount identity' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set registrationGateway.enabled=true \
  --set registrationGateway.gatewayName=public \
  --set registrationGateway.gatewayNamespace=gateway-system \
  --set registrationGateway.listenerName=tls \
  --set registrationGateway.telegramHostname=telegram.example.com \
  --set registrationGateway.sentryHostname=sentry.example.com >"$tmp_dir/negative.log" 2>&1; then
  echo 'Gateway routes accepted disabled registration workload' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set registration.enabled=true \
  --set registrationGateway.enabled=true >"$tmp_dir/negative.log" 2>&1; then
  echo 'Gateway routes accepted missing parent and public hostname values' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.35.0 \
  --set registration.enabled=true \
  --set registrationGateway.enabled=true \
  --set registrationGateway.gatewayName=public \
  --set registrationGateway.gatewayNamespace=gateway-system \
  --set registrationGateway.listenerName=tls \
  --set registrationGateway.telegramHostname=shared.example.com \
  --set registrationGateway.sentryHostname=shared.example.com >"$tmp_dir/negative.log" 2>&1; then
  echo 'Gateway routes accepted ambiguous duplicate SNI hostname' >&2
  exit 1
fi
if "$helm_bin" template sentry-operator "$chart_dir" --kube-version 1.34.9 >"$tmp_dir/negative.log" 2>&1; then
  echo 'chart accepted unsupported Kubernetes 1.34' >&2
  exit 1
fi

"$helm_bin" package "$chart_dir" --destination "$tmp_dir" >/dev/null
test -f "$tmp_dir/sentry-kubernetes-operator-0.1.0-alpha.1.tgz"
tar -tzf "$tmp_dir/sentry-kubernetes-operator-0.1.0-alpha.1.tgz" \
  | rg -q '^sentry-kubernetes-operator/LICENSE$'
tar -tzf "$tmp_dir/sentry-kubernetes-operator-0.1.0-alpha.1.tgz" \
  | rg -q '^sentry-kubernetes-operator/NOTICE$'

echo 'Helm chart drift, non-mutating image render, schema, Kubernetes 1.35/1.36, CRD, opt-in and immutable-image contracts verified'
