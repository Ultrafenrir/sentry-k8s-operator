#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${SENTRY_VERSION:-26.10.1}
cache_dir=${SENTRY_SELF_HOSTED_CACHE:-$root_dir/.cache/sentry-self-hosted}
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}

case "$version" in
  24.7.1) port=${SENTRY_PORT:-24710} ;;
  26.10.1) port=${SENTRY_PORT:-26101} ;;
  *) echo "unsupported Sentry version: $version" >&2; exit 1 ;;
esac
case "$cache_dir" in
  /*) ;;
  *) echo 'SENTRY_SELF_HOSTED_CACHE must be an absolute path' >&2; exit 1 ;;
esac

token_file="$cache_dir/tokens/$version.token"
if [ ! -x "$go_bin" ] || [ ! -d "$envtest_assets" ] || [ ! -f "$token_file" ]; then
  echo 'live controller acceptance prerequisites are missing' >&2
  exit 1
fi

version_slug=$(printf '%s' "$version" | tr '.' '-')
cd "$root_dir"
SENTRY_LIVE_BASE_URL="http://127.0.0.1:$port" \
SENTRY_LIVE_TOKEN_FILE="$token_file" \
SENTRY_LIVE_VERSION="$version" \
SENTRY_LIVE_ORGANIZATION="sko-live-$version_slug" \
SENTRY_LIVE_TEAM='sko-live' \
KUBEBUILDER_ASSETS="$envtest_assets" \
GOMODCACHE="$product_gomodcache" \
GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/controller --ginkgo.label-filter=live-sentry -count=1

echo "Sentry $version controller lost-response recovery acceptance passed"
