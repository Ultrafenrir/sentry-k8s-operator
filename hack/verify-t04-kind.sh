#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-t04-secret-ref

cleanup() {
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

kubectl --context "$context" create namespace "$namespace" >/dev/null
kubectl --context "$context" -n "$namespace" create deployment consumer \
  --image=busybox:t00-arm64 -- /bin/sh -c 'exec sleep 3600' >/dev/null
kubectl --context "$context" -n "$namespace" patch deployment consumer --type=strategic -p \
  '{"spec":{"template":{"spec":{"containers":[{"name":"busybox","imagePullPolicy":"IfNotPresent","env":[{"name":"SENTRY_DSN","valueFrom":{"secretKeyRef":{"name":"checkout-sentry","key":"SENTRY_DSN","optional":false}}}]}]}}}}' >/dev/null

deadline=$(( $(date +%s) + 45 ))
while :; do
  reason=$(kubectl --context "$context" -n "$namespace" get pods -l app=consumer -o jsonpath='{.items[0].status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
  [ "$reason" = "CreateContainerConfigError" ] && break
  [ "$(date +%s)" -ge "$deadline" ] && {
    kubectl --context "$context" -n "$namespace" get pods -o wide
    echo "workload did not block on missing required Secret; last reason=$reason" >&2
    exit 1
  }
  sleep 1
done

kubectl --context "$context" -n "$namespace" create secret generic checkout-sentry \
  --from-literal='SENTRY_DSN=https://public@example.invalid/42' >/dev/null
kubectl --context "$context" -n "$namespace" rollout status deployment/consumer --timeout=60s >/dev/null

echo 'T04 kind required secretKeyRef blocks before Secret and starts after Secret'
