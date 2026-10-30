#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mode=${1:?mode must be verify, deploy, or delete}
kubectl_bin=${KUBECTL:-kubectl}
route_namespace=sentry-kubernetes-operator-system
timeout=${GATEWAY_WAIT_TIMEOUT:-120s}
case "$timeout" in
  *s) wait_seconds=${timeout%s} ;;
  *) echo 'GATEWAY_WAIT_TIMEOUT must be an integer number of seconds ending in s' >&2; exit 1 ;;
esac
case "$wait_seconds" in
  ''|*[!0-9]*) echo 'GATEWAY_WAIT_TIMEOUT must be an integer number of seconds ending in s' >&2; exit 1 ;;
esac
if [ "$wait_seconds" -lt 1 ] || [ "$wait_seconds" -gt 600 ]; then
  echo 'GATEWAY_WAIT_TIMEOUT must be between 1s and 600s' >&2
  exit 1
fi

render() {
  sh "$root_dir/hack/render-registration-gateway.sh"
}

verify_prerequisites() {
  served=$("$kubectl_bin" get crd tlsroutes.gateway.networking.k8s.io \
    -o 'jsonpath={.spec.versions[?(@.name=="v1")].served}')
  if [ "$served" != true ]; then
    echo 'TLSRoute gateway.networking.k8s.io/v1 is not served; install Gateway API Standard Channel v1.5 or newer' >&2
    exit 1
  fi

  listener=$("$kubectl_bin" -n "$GATEWAY_NAMESPACE" get gateway "$GATEWAY_NAME" \
    -o "jsonpath={.spec.listeners[?(@.name==\"$GATEWAY_LISTENER\")].protocol}{'|'}{.spec.listeners[?(@.name==\"$GATEWAY_LISTENER\")].tls.mode}")
  if [ "$listener" != 'TLS|Passthrough' ]; then
    echo "Gateway $GATEWAY_NAMESPACE/$GATEWAY_NAME listener $GATEWAY_LISTENER must be TLS/Passthrough, got ${listener:-missing}" >&2
    exit 1
  fi

  "$kubectl_bin" -n "$route_namespace" get service/telegram-receiver service/sentry-hook-receiver >/dev/null
  render | "$kubectl_bin" apply --dry-run=server -f - >/dev/null
}

verify_route_status() {
  route=$1
  condition=$2
  attempt=0
  status=
  while [ "$attempt" -lt "$wait_seconds" ]; do
    attempt=$((attempt + 1))
    status=$("$kubectl_bin" -n "$route_namespace" get tlsroute "$route" \
      -o "jsonpath={.status.parents[0].conditions[?(@.type==\"$condition\")].status}" 2>/dev/null || true)
    [ "$status" = True ] && break
    sleep 1
  done
  if [ "$status" != True ]; then
    echo "TLSRoute $route did not reach $condition=True within $timeout" >&2
    "$kubectl_bin" -n "$route_namespace" get tlsroute "$route" -o yaml >&2 || true
    "$kubectl_bin" -n "$GATEWAY_NAMESPACE" get gateway "$GATEWAY_NAME" -o yaml >&2 || true
    exit 1
  fi
  parent=$("$kubectl_bin" -n "$route_namespace" get tlsroute "$route" \
    -o 'jsonpath={.status.parents[0].parentRef.namespace}{"|"}{.status.parents[0].parentRef.name}{"|"}{.status.parents[0].parentRef.sectionName}')
  expected="$GATEWAY_NAMESPACE|$GATEWAY_NAME|$GATEWAY_LISTENER"
  if [ "$parent" != "$expected" ]; then
    echo "TLSRoute $route reported unexpected parent ${parent:-missing}; expected $expected" >&2
    exit 1
  fi
}

case "$mode" in
  verify)
    verify_prerequisites
    ;;
  deploy)
    verify_prerequisites
    render | "$kubectl_bin" apply -f -
    verify_route_status telegram-receiver Accepted
    verify_route_status telegram-receiver ResolvedRefs
    verify_route_status sentry-hook-receiver Accepted
    verify_route_status sentry-hook-receiver ResolvedRefs
    ;;
  delete)
    render | "$kubectl_bin" delete --ignore-not-found="${ignore_not_found:-false}" -f -
    ;;
  *)
    echo "unsupported mode: $mode" >&2
    exit 1
    ;;
esac
