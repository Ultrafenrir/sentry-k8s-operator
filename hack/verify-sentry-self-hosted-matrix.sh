#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
manager=$root_dir/hack/manage-sentry-self-hosted.sh
versions=${SENTRY_VERSIONS:-'24.7.1 26.10.1'}
current_version=

cleanup() {
  if [ -n "$current_version" ]; then
    SENTRY_VERSION="$current_version" sh "$manager" down >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

for current_version in $versions; do
  SENTRY_VERSION="$current_version" sh "$manager" up
  SENTRY_VERSION="$current_version" sh "$manager" bootstrap
  SENTRY_VERSION="$current_version" sh "$manager" smoke
  SENTRY_VERSION="$current_version" sh "$manager" down
  current_version=
done

echo "Fresh self-hosted Sentry matrix passed: $versions"
