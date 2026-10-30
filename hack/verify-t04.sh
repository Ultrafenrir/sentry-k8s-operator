#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

cd "$root_dir"
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/dsn ./internal/binding
KUBEBUILDER_ASSETS="$envtest_assets" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/controller
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/dsn ./internal/binding ./internal/controller ./cmd

ruby -ryaml -rjson -e '
  document = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
  puts JSON.generate(document)
' config/samples/workload_with_sentry_dsn.yaml >"$tmp_dir/workload.json"
jq -e '
  .kind == "Deployment" and
  .spec.template.spec.containers[0].env == [{
    "name": "SENTRY_DSN",
    "valueFrom": {"secretKeyRef": {"key": "SENTRY_DSN", "name": "checkout-sentry", "optional": false}}
  }]
' "$tmp_dir/workload.json" >/dev/null

ruby -ryaml -rjson -e '
  documents = YAML.load_stream(File.read(ARGV.fetch(0)))
  role = documents.find { |document| document.is_a?(Hash) && document["kind"] == "ClusterRole" && document.dig("metadata", "name") == "manager-role" }
  abort "manager role not found" unless role
  puts JSON.generate(role)
' config/rbac/role.yaml >"$tmp_dir/manager-role.json"
jq -e '
  any(.rules[];
    .apiGroups == [""] and .resources == ["secrets"] and
    ((.verbs | sort) == ["create", "delete", "get", "patch", "update"])
  )
' "$tmp_dir/manager-role.json" >/dev/null

for fixture in testdata/http-fixtures/sentry-24.7.1/client-keys-response.json testdata/http-fixtures/sentry-26.10.1/client-keys-response.json; do
  jq -e 'length >= 1 and all(.[]; has("id") and has("isActive") and .dsn.public)' "$fixture" >/dev/null
done

if rg -n -g '!**/*_test.go' 'log\.[A-Za-z]+\([^\n]*(DSN|dsn\.public)|fmt\.Print[^\n]*(DSN|dsn\.public)' internal cmd; then
  echo 'possible DSN logging found' >&2
  exit 1
fi

rg -q 'http.MethodGet' internal/sentryapi/pagination.go
if rg -n 'http\.Method(Post|Put|Patch|Delete).*keys|/keys/.*http\.Method(Post|Put|Patch|Delete)' internal; then
  echo 'client key mutation route found' >&2
  exit 1
fi
rg -q 'confirm-dsn-cutover' internal/dsn/publisher.go
rg -q 'OutputSecrets' internal/controller/sentryproject_controller.go
rg -q 'DSNClientKey string.*dsnClientKey' api/v1alpha1/sentryproject_types.go
rg -q 'ExternalNameChanged' internal/controller/sentryproject_controller.go
rg -q 'persistent readable client-key rename' internal/controller/sentryproject_controller_test.go
ruby -ryaml -e '
  crd = YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true)
  status = crd.fetch("spec").fetch("versions").fetch(0).fetch("schema").fetch("openAPIV3Schema").fetch("properties").fetch("status").fetch("properties")
  key = status.fetch("dsnClientKey")
  abort "dsnClientKey must be a bounded string" unless key["type"] == "string" && key["maxLength"] == 253
  abort "external DSN identity leaked into status schema" if status.key?("dsnClientKeyID") || status.key?("dsn")
' config/crd/bases/sentryconfiguration.io_sentryprojects.yaml

echo 'T04 DSN pagination, persistent rename status, pinning, output Secret, revoke and workload contracts verified'
