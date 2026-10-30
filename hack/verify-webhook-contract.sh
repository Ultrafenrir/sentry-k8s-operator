#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
module_dir="$root_dir/tools/t00-webhook-probe"

(
  cd "$module_dir"
  REPO_ROOT="$root_dir" \
  GOMODCACHE="$root_dir/.tools/gomodcache" \
  GOCACHE="$root_dir/.tools/gocache" \
  GOTOOLCHAIN=local \
    "$go_bin" test -count=1 ./...
)
