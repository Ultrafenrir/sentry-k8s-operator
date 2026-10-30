#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

test "$(cd "$root_dir" && GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" list -m -f '{{.Version}}' sigs.k8s.io/controller-runtime)" = v0.24.1
test "$(cd "$root_dir" && GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" list -m -f '{{.Version}}' k8s.io/apimachinery)" = v0.36.0
test "$(find "$root_dir/testdata/t01/golden" -type f -name '*.yaml' | wc -l | tr -d ' ')" = 6

ruby -ryaml -rjson -e '
  puts JSON.generate(YAML.load_file(ARGV.fetch(0)))
' "$root_dir/config/rbac/role.yaml" >"$tmp_dir/rbac.json"
jq -e '
  any(.rules[];
    .resources == ["sentryconnections"] and
    (.verbs | sort) == ["get", "list", "watch"]) and
  any(.rules[];
    .resources == ["sentryprojects"] and
    (.verbs | sort) == ["get", "list", "patch", "watch"]) and
  any(.rules[];
    (.resources | sort) == ["sentryconnections/status", "sentryprojects/status"] and
    (.verbs | sort) == ["get", "patch", "update"]) and
  all(.rules[];
    all(.resources[]; endswith("/finalizers") | not)) and
  all(.rules[];
    ((.resources == ["sentryprojects"]) | not) or
    ((.verbs | index("create") | not) and (.verbs | index("delete") | not) and
     (.verbs | index("update") | not)))
' "$tmp_dir/rbac.json" >/dev/null

for crd in "$root_dir"/config/crd/bases/*.yaml; do
  ruby -ryaml -rjson -e '
    puts JSON.generate(YAML.load_file(ARGV.fetch(0)))
  ' "$crd" >"$tmp_dir/$(basename "$crd").json"
  jq -e '
    (.spec.conversion == null or .spec.conversion.strategy == "None") and
    ([.spec.versions[] | select(.served == true and .storage == true) | .name] == ["v1alpha1"])
  ' "$tmp_dir/$(basename "$crd").json" >/dev/null
done

ruby -ryaml -rjson -e '
  YAML.load_stream(File.read(ARGV.fetch(0))).compact.each { |document| puts JSON.generate(document) }
' "$root_dir/config/webhook/manifests.yaml" | jq -s '{items: .}' >"$tmp_dir/webhooks.json"
jq -e '
  (.items | length == 2) and
  all(.items[].webhooks[];
    .failurePolicy == "Fail" and .sideEffects == "None" and
    .admissionReviewVersions == ["v1"])
' "$tmp_dir/webhooks.json" >/dev/null

if rg -n '(^|[[:space:]])(profile|grouping|advanced):' \
  "$root_dir/testdata/t01/golden" "$root_dir/config/samples"; then
  echo 'unpublished alpha field found in positive examples' >&2
  exit 1
fi

echo 'T01 generated dependency, CRD, RBAC, webhook and golden contracts verified'
