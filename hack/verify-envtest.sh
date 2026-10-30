#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
module_dir="$root_dir/tools/t00-envtest-probe"

for version in 1.35.0 1.36.2; do
  echo "envtest $version"
  (
    cd "$module_dir"
    REPO_ROOT="$root_dir" \
    KUBEBUILDER_ASSETS="$root_dir/.tools/envtest/$version" \
    GOMODCACHE="$root_dir/.tools/gomodcache" \
    GOCACHE="$root_dir/.tools/gocache" \
    GOTOOLCHAIN=local \
      "$go_bin" test -count=1 ./...
  )
done
