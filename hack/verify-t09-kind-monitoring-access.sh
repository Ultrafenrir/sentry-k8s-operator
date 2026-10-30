#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=t09-monitoring-access-probe
service_account=prometheus-probe
binding=sentry-kubernetes-operator-controller-metrics-reader
created_namespace=false
created_binding=false

case "$context" in
  kind-*) ;;
  *) echo "refusing non-kind context: $context" >&2; exit 1 ;;
esac
kubectl --context "$context" get namespace sentry-kubernetes-operator-system >/dev/null
if kubectl --context "$context" get clusterrolebinding "$binding" >/dev/null 2>&1; then
  echo "refusing to replace existing ClusterRoleBinding: $binding" >&2
  exit 1
fi
if kubectl --context "$context" get namespace "$namespace" >/dev/null 2>&1; then
  echo "refusing to reuse existing namespace: $namespace" >&2
  exit 1
fi

cleanup() {
  if [ "$created_binding" = true ]; then
    kubectl --context "$context" delete clusterrolebinding "$binding" --ignore-not-found >/dev/null 2>&1 || true
  fi
  if [ "$created_namespace" = true ]; then
    kubectl --context "$context" delete namespace "$namespace" --wait=true --timeout=120s >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

kubectl --context "$context" create namespace "$namespace" >/dev/null
created_namespace=true
kubectl --context "$context" -n "$namespace" create serviceaccount "$service_account" >/dev/null

PROMETHEUS_SERVICE_ACCOUNT_NAME="$service_account" \
PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE="$namespace" \
KUBECTL_CONTEXT="$context" \
  sh "$root_dir/hack/manage-controller-monitoring-access.sh" deploy >/dev/null
created_binding=true

subject=$(kubectl --context "$context" get clusterrolebinding "$binding" \
  -o 'jsonpath={.subjects[0].kind}{"|"}{.subjects[0].namespace}{"|"}{.subjects[0].name}{"|"}{.roleRef.name}')
test "$subject" = "ServiceAccount|$namespace|$service_account|sentry-kubernetes-operator-metrics-reader"

metrics_access=$(kubectl --context "$context" auth can-i get /metrics \
  --as="system:serviceaccount:$namespace:$service_account")
test "$metrics_access" = yes
secret_access=$(kubectl --context "$context" auth can-i get secrets \
  -n sentry-kubernetes-operator-system \
  --as="system:serviceaccount:$namespace:$service_account" || true)
test "$secret_access" = no

echo "$context: exact Prometheus ServiceAccount can GET /metrics and cannot get operator Secrets; probe binding/namespace cleanup armed"
