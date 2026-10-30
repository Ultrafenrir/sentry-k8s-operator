#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
chart_dir=${CHART_DIR:-"$root_dir/charts/sentry-kubernetes-operator"}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

mkdir -p "$chart_dir/files" "$chart_dir/crds"

kubectl kustomize "$root_dir/config/state" >"$chart_dir/files/state.yaml"
kubectl kustomize "$root_dir/config/default" >"$tmp_dir/default.yaml"
awk '
  function flush() {
    if (document != "" && document !~ /kind: CustomResourceDefinition/) {
      printf "%s", document
    }
    document = ""
  }
  /^---[[:space:]]*$/ { flush(); document = $0 ORS; next }
  { document = document $0 ORS }
  END { flush() }
' "$tmp_dir/default.yaml" >"$chart_dir/files/operator.yaml"
kubectl kustomize "$root_dir/config/registration" >"$chart_dir/files/registration.yaml"
cp "$root_dir/config/registration-gateway/tlsroutes.yaml" "$chart_dir/files/registration-gateway.yaml"
kubectl kustomize "$root_dir/config/registration-monitoring" >"$chart_dir/files/registration-monitoring.yaml"
kubectl kustomize "$root_dir/config/controller-monitoring" >"$chart_dir/files/controller-monitoring.yaml"
cp "$root_dir/config/controller-monitoring-access/cluster-role-binding.yaml" "$chart_dir/files/controller-monitoring-access.yaml"
cp "$root_dir"/config/crd/bases/*.yaml "$chart_dir/crds/"
