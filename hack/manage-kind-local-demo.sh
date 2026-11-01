#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
action=${1:-}
kubectl_bin=${KUBECTL:-kubectl}
kind_context=${KIND_CONTEXT:-kind-sentry-operator-dev}
cache_dir=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}
version=${SENTRY_VERSION:-26.10.1}
operator_namespace=sentry-kubernetes-operator-system
demo_label=sentryconfiguration.io/demo=kind-local

case "$action" in
  apply|status|clean) ;;
  *) echo 'usage: manage-kind-local-demo.sh apply|status|clean' >&2; exit 2 ;;
esac

if [ "$version" != 26.10.1 ]; then
  echo 'the bounded local demo supports only the documented Sentry 26.10.1 target' >&2
  exit 1
fi
case "$kubectl_bin" in
  */*) [ -x "$kubectl_bin" ] || { echo "kubectl is not executable: $kubectl_bin" >&2; exit 1; } ;;
  *) command -v "$kubectl_bin" >/dev/null 2>&1 || { echo "kubectl is not on PATH: $kubectl_bin" >&2; exit 1; } ;;
esac
command -v jq >/dev/null 2>&1 || { echo 'jq is not on PATH' >&2; exit 1; }
case "$kind_context" in
  kind-*) ;;
  *) echo "refusing non-kind context: $kind_context" >&2; exit 1 ;;
esac
if ! "$kubectl_bin" config get-contexts "$kind_context" -o name | grep -Fxq "$kind_context"; then
  echo "kind context does not exist: $kind_context" >&2
  exit 1
fi

kube() {
  "$kubectl_bin" --context "$kind_context" "$@"
}

show_status() {
  kube get sentryconnections
  kube get sentryprojects -A \
    -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason'
  kube -n sentry-demo-modern get secrets modern-demo-dsn readonly-demo-dsn
}

case "$action" in
  status)
    show_status
    ;;
  apply)
    token_file="$cache_dir/tokens/$version.token"
    evidence_file="$cache_dir/evidence/$version-capabilities.json"
    if [ ! -f "$token_file" ] || [ "$(stat -f '%Lp' "$token_file")" != 600 ]; then
      echo "run make sentry-bootstrap SENTRY_VERSION=$version first; the mode-0600 token is missing" >&2
      exit 1
    fi
    if [ ! -f "$evidence_file" ] || [ "$(stat -f '%Lp' "$evidence_file")" != 600 ]; then
      echo "run make sentry-live-config-api SENTRY_VERSION=$version first; capability evidence is missing" >&2
      exit 1
    fi
    jq -e --arg version "$version" --arg organization "sko-live-26-10-1" '
      .version == $version and
      .organization == $organization and
      (.gate == "sentry-live-config-api" or .gate == "sentry-live-api") and
      .capabilities == [
        "project.create.atomic-default-rules",
        "project.create.automatic-active-key"
      ]
    ' "$evidence_file" >/dev/null
    kube -n "$operator_namespace" get deployment/sentry-kubernetes-operator-controller-manager >/dev/null
    kube -n "$operator_namespace" create secret generic demo-sentry-api \
      --from-file="token=$token_file" --dry-run=client -o yaml | kube apply -f -
    kube apply -f "$root_dir/config/demo/kind-local.yaml"
    kube wait sentryconnection/demo-modern --for=condition=CredentialsReady --timeout=120s
    kube wait sentryconnection/demo-legacy --for=condition=CredentialsReady --timeout=120s
    capability_status=$(jq -c '{status:{serverVersion:.version,capabilities:.capabilities}}' "$evidence_file")
    kube patch sentryconnection/demo-modern --subresource=status --type=merge -p "$capability_status"
    kube patch sentryconnection/demo-legacy --subresource=status --type=merge -p "$capability_status"
    kube -n sentry-demo-modern wait sentryproject/modern-demo --for=condition=ProjectReady --timeout=300s
    kube -n sentry-demo-modern wait sentryproject/modern-demo --for=condition=DSNReady --timeout=300s
    kube -n sentry-demo-modern wait sentryproject/modern-demo --for=condition=SettingsReady --timeout=300s
    kube apply -f "$root_dir/config/demo/kind-local-readonly.yaml"
    kube -n sentry-demo-modern wait sentryproject/readonly-demo --for=condition=Ready --timeout=300s
    show_status
    ;;
  clean)
    kube -n sentry-demo-modern delete sentryproject/readonly-demo --ignore-not-found --wait=true --timeout=300s
    kube -n sentry-demo-modern delete sentryproject/modern-demo --ignore-not-found --wait=true --timeout=300s
    kube -n sentry-demo-surface delete sentryproject/destination-surface --ignore-not-found --wait=true --timeout=300s
    kube -n sentry-demo-legacy delete sentryproject/legacy-demo --ignore-not-found --wait=true --timeout=300s
    kube -n sentry-demo-paused delete sentryproject/paused-demo --ignore-not-found --wait=true --timeout=300s
    kube delete sentryconnection/demo-modern sentryconnection/demo-legacy --ignore-not-found --wait=true --timeout=120s
    for namespace in sentry-demo-modern sentry-demo-legacy sentry-demo-surface sentry-demo-paused; do
      if kube get namespace "$namespace" -o jsonpath='{.metadata.labels.sentryconfiguration\.io/demo}' 2>/dev/null | grep -Fxq kind-local; then
        kube delete namespace "$namespace" --wait=true --timeout=300s
      elif kube get namespace "$namespace" >/dev/null 2>&1; then
        echo "refusing to delete namespace without $demo_label: $namespace" >&2
        exit 1
      fi
    done
    kube -n "$operator_namespace" delete secret/demo-sentry-api --ignore-not-found
    ;;
esac
