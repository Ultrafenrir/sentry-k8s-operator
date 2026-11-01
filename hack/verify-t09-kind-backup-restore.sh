#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_context=${1:-}
kubectl_bin=${KUBECTL:-kubectl}
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
ownership_label='sentryconfiguration.io/test=statebackup-kind-drill'
namespaces='sko-backup-drill-state sko-backup-drill-routing sko-backup-drill-delivery sko-backup-drill-hook-sources'
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/sko-kind-backup-restore.XXXXXX")
drill_kubeconfig="$temporary_dir/kubeconfig"

cleanup() {
  for namespace in $namespaces; do
    if "$kubectl_bin" --kubeconfig "$drill_kubeconfig" get namespace "$namespace" -o jsonpath='{.metadata.labels.sentryconfiguration\.io/test}' 2>/dev/null | grep -Fxq statebackup-kind-drill; then
      "$kubectl_bin" --kubeconfig "$drill_kubeconfig" delete namespace "$namespace" --wait=true --timeout=120s >/dev/null 2>&1 || true
    fi
  done
  rm -rf "$temporary_dir"
}
trap cleanup EXIT INT TERM

case "$kind_context" in
  kind-*) ;;
  *) echo 'KIND_CONTEXT must name an explicit kind-* context' >&2; exit 2 ;;
esac
"$kubectl_bin" config get-contexts "$kind_context" >/dev/null
for namespace in $namespaces; do
  if "$kubectl_bin" --context "$kind_context" get namespace "$namespace" >/dev/null 2>&1; then
    echo "refusing to reuse existing namespace: $namespace" >&2
    exit 1
  fi
done
"$kubectl_bin" --context "$kind_context" config view --minify --flatten >"$drill_kubeconfig"
chmod 600 "$drill_kubeconfig"

cd "$root_dir"
USE_EXISTING_CLUSTER=true KUBECONFIG="$drill_kubeconfig" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/statebackup -run '^TestProtectedStateRoundTripAgainstAPIServer$' -count=1

for namespace in $namespaces; do
  if ! "$kubectl_bin" --kubeconfig "$drill_kubeconfig" get namespace "$namespace" -o jsonpath='{.metadata.labels.sentryconfiguration\.io/test}' | grep -Fxq statebackup-kind-drill; then
    echo "drill namespace lost exact ownership label: $namespace" >&2
    exit 1
  fi
done

echo 'T09 protected-state in-memory export, exact deletion, create-only restore, resumable restore, digest stability, and conflict rejection passed against kind'
