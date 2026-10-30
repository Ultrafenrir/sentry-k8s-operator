#!/bin/sh
set -eu

context=${1:-kind-sentry-operator-dev}
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"

(
  cd "$root_dir/tools/t00-envtest-probe"
  GOMODCACHE="$root_dir/.tools/gomodcache" \
  GOCACHE="$root_dir/.tools/gocache" \
  GOTOOLCHAIN=local \
    "$go_bin" run ./cmd/leader-probe --context "$context"
)
