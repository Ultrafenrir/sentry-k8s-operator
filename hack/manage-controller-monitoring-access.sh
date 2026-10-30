#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:?mode must be verify, deploy, or delete}
kubectl_bin=${KUBECTL:-kubectl}
kubectl_context=${KUBECTL_CONTEXT:-}

kubectl_call() {
  if [ -n "$kubectl_context" ]; then
    "$kubectl_bin" --context "$kubectl_context" "$@"
  else
    "$kubectl_bin" "$@"
  fi
}

render() {
  sh "$root_dir/hack/render-controller-monitoring-access.sh"
}

verify_prerequisites() {
  kubectl_call -n "$PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE" get serviceaccount "$PROMETHEUS_SERVICE_ACCOUNT_NAME" >/dev/null
  kubectl_call get clusterrole sentry-kubernetes-operator-metrics-reader >/dev/null
  render | kubectl_call apply --dry-run=server -f - >/dev/null
}

case "$mode" in
  verify)
    verify_prerequisites
    ;;
  deploy)
    verify_prerequisites
    render | kubectl_call apply -f -
    ;;
  delete)
    render | kubectl_call delete --ignore-not-found="${ignore_not_found:-false}" -f -
    ;;
  *)
    echo "unsupported mode: $mode" >&2
    exit 1
    ;;
esac
