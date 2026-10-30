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

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/audit ./internal/binding ./internal/project
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/audit ./internal/binding ./internal/project ./internal/controller

ruby -ryaml -rjson -e '
  documents = YAML.load_stream(File.read(ARGV.fetch(0)))
  role = documents.find { |document| document.is_a?(Hash) && document["kind"] == "Role" }
  abort "protected Role is missing" unless role
  puts JSON.generate(role)
' config/state/rbac.yaml >"$tmp_dir/protected-role.json"
jq -e '
  .kind == "Role" and .metadata.namespace == "sentry-kubernetes-operator-state" and
  (.rules | length == 1) and .rules[0].resources == ["secrets"] and
  ((.rules[0].verbs | sort) == ["create", "delete", "get", "list", "patch", "update", "watch"])
' "$tmp_dir/protected-role.json" >/dev/null

for fixture in testdata/http-fixtures/sentry-24.7.1/project-create-request.json testdata/http-fixtures/sentry-26.10.1/project-create-request.json; do
  jq -e '.default_rules == false and (.name | test(" \\[managed;o=[a-z0-9.-]+;m=[a-z0-9.-]+\\]$"))' "$fixture" >/dev/null
done

if rg -n 'ExternalIdentity|externalIdentity|ManagerUID|managerUID' api config/crd/bases config/samples testdata/t01; then
  echo 'protected identity leaked into public API artifacts' >&2
  exit 1
fi

rg -q 'sentryconfiguration.io/project-protection' internal/controller/sentryproject_controller.go
test "$(rg -c 'r\.Patch\(ctx, resource, client\.MergeFrom\(base\)\)' internal/controller/sentryproject_controller.go)" -eq 2
test "$(rg -c 'base := resource\.DeepCopy\(\)' internal/controller/sentryproject_controller.go)" -ge 2
kubectl kustomize config/default | rg -q -- '--state-namespace=sentry-kubernetes-operator-state'
kubectl kustomize config/state | rg -q 'name: sentry-kubernetes-operator-state'
rg -q 'stateNamespace:[[:space:]]+\{\}' cmd/main.go
rg -q 'ManagedCreateUnsupported' internal/controller/sentryproject_controller.go
rg -q 'RetrySafe:[[:space:]]+false' internal/project/http_adapter.go
rg -q 'DefaultRules:[[:space:]]+false' internal/project/engine.go
rg -q 'TestAuditFailureBeforeCreateBlocksPOSTAndKeepsSubmittedIntent' internal/project/engine_test.go
rg -q 'TestAuditFailureAfterCreateResponseRecoversWithoutSecondPOST' internal/project/engine_test.go
rg -q 'TestAuditFailureAfterDeleteResponseRecoversWithoutSecondDelete' internal/project/engine_test.go

echo 'T03 ownership, binding, one-POST recovery, finalizer and guarded deletion contracts verified'
