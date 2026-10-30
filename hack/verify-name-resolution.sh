#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
module_dir="$root_dir/tools/t00-name-resolution-probe"

(
  cd "$module_dir"
  GOMODCACHE="$root_dir/.tools/gomodcache" \
  GOCACHE="$root_dir/.tools/gocache" \
  GOTOOLCHAIN=local \
    "$root_dir/.tools/go-1.26.10/bin/go" test -count=1 ./...
)
