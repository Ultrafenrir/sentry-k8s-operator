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
  "$go_bin" test -race ./internal/sentryapi ./internal/resolver ./internal/policy ./internal/connection
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/sentryapi ./internal/resolver ./internal/policy ./internal/connection ./internal/controller

kubectl create --dry-run=client --validate=false -f config/rbac/secret_reader_role.yaml -o json >"$tmp_dir/secret-role.json"
jq -e '
  .kind == "Role" and .metadata.namespace == "system" and
  (.rules | length == 1) and
  (.rules[0].resources == ["secrets"]) and
  ((.rules[0].verbs | sort) == ["get", "list", "watch"])
' "$tmp_dir/secret-role.json" >/dev/null

kubectl create --dry-run=client --validate=false -f config/rbac/role.yaml -o json >"$tmp_dir/manager-role.json"
jq -e '
  all(.rules[];
    if (.resources | index("secrets")) then
      .apiGroups == [""] and .resources == ["secrets"] and
      ((.verbs | sort) == ["create", "delete", "get", "patch", "update"])
    else true end
  )
' "$tmp_dir/manager-role.json" >/dev/null

if rg -n -g '!**/*_test.go' 'InsecureSkipVerify[[:space:]]*:[[:space:]]*true' internal; then
  echo 'insecure TLS bypass found' >&2
  exit 1
fi

rg -q 'CheckRedirect:' internal/sentryapi/client.go
rg -q 'fieldPath: metadata.namespace' config/manager/manager.yaml
rg -q 'Watches\(&corev1.Secret' internal/controller/sentryconnection_controller.go

echo 'T02 client, resolver, policy, secret-rotation and least-privilege contracts verified'
