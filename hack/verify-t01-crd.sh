#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-operator-t01-validation
project_crd=sentryprojects.sentryconfiguration.io
connection_crd=sentryconnections.sentryconfiguration.io
tmp_dir=$(mktemp -d)

cleanup() {
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl --context "$context" delete crd "$project_crd" "$connection_crd" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

if kubectl --context "$context" get crd "$project_crd" >/dev/null 2>&1 || \
    kubectl --context "$context" get crd "$connection_crd" >/dev/null 2>&1; then
  echo 'refusing to replace pre-existing product CRDs in the selected context' >&2
  exit 1
fi

kubectl --context "$context" apply -f "$root_dir/config/crd/bases" >/dev/null
kubectl --context "$context" create namespace "$namespace" >/dev/null

golden_count=$(find "$root_dir/testdata/t01/golden" -type f -name '*.yaml' | wc -l | tr -d ' ')
test "$golden_count" = 5

for fixture in "$root_dir"/testdata/t01/golden/*.yaml; do
  rendered="$tmp_dir/$(basename "$fixture")"
  sed "s/namespace: .*/namespace: $namespace/" "$fixture" >"$rendered"
  kubectl --context "$context" create --dry-run=server --validate=strict \
    -f "$rendered" -o json >"$rendered.json"
done

jq -e '.spec.paused == false and .spec.lifecycle.onDelete == "Retain"' \
  "$tmp_dir/01-managed-minimal.yaml.json" >/dev/null
jq -e '
  .spec.settings.filters.localhost == false and
  .spec.settings.pii.scrubIPAddresses == false and
  (.spec.settings.pii.safeFields | type == "array" and length == 0) and
  (.spec.settings.pii.sensitiveFields | length == 2)
' "$tmp_dir/04-managed-settings.yaml.json" >/dev/null

for fixture in "$root_dir"/testdata/t01/negative/*.yaml; do
  rendered="$tmp_dir/negative-$(basename "$fixture")"
  sed "s/namespace: .*/namespace: $namespace/" "$fixture" >"$rendered"
  if kubectl --context "$context" create --dry-run=server --validate=strict \
    -f "$rendered" >"$rendered.out" 2>&1; then
    echo "negative fixture unexpectedly passed: $(basename "$fixture")" >&2
    exit 1
  fi
done

echo "$context: five T01 golden fixtures and all schema/CEL negatives passed"
