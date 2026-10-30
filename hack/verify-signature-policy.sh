#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp_dir=$(mktemp -d)
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

image="ghcr.io/example/sentry-kubernetes-operator@sha256:$(printf 'a%.0s' $(seq 1 64))"
identity=https://github.com/example/sentry-kubernetes-operator/.github/workflows/release.yml@refs/tags/v1.2.3
args_file="$tmp_dir/args"

COSIGN_ARGS_FILE="$args_file" COSIGN_BIN="$root_dir/hack/testdata/fake-cosign.sh" \
  COSIGN_CERTIFICATE_IDENTITY="$identity" IMG="$image" \
  sh "$root_dir/hack/verify-signed-image.sh"

test "$(sed -n '1p' "$args_file")" = verify
test "$(sed -n '2p' "$args_file")" = --certificate-identity
test "$(sed -n '3p' "$args_file")" = "$identity"
test "$(sed -n '4p' "$args_file")" = --certificate-oidc-issuer
test "$(sed -n '5p' "$args_file")" = https://token.actions.githubusercontent.com
test "$(sed -n '6p' "$args_file")" = "$image"
test "$(wc -l <"$args_file" | tr -d ' ')" = 6

if COSIGN_ARGS_FILE="$args_file" COSIGN_BIN="$root_dir/hack/testdata/fake-cosign.sh" \
  COSIGN_CERTIFICATE_IDENTITY="$identity" IMG=ghcr.io/example/operator:latest \
  sh "$root_dir/hack/verify-signed-image.sh" >/dev/null 2>&1; then
  echo 'mutable image unexpectedly passed signature policy' >&2
  exit 1
fi
if COSIGN_ARGS_FILE="$args_file" COSIGN_BIN="$root_dir/hack/testdata/fake-cosign.sh" \
  COSIGN_CERTIFICATE_IDENTITY='.*' IMG="$image" \
  sh "$root_dir/hack/verify-signed-image.sh" >/dev/null 2>&1; then
  echo 'broad certificate identity unexpectedly passed signature policy' >&2
  exit 1
fi
if COSIGN_ARGS_FILE="$args_file" COSIGN_BIN="$root_dir/hack/testdata/fake-cosign.sh" \
  COSIGN_CERTIFICATE_IDENTITY='https://github.com/example/sentry-kubernetes-operator/.github/workflows/release.yml@refs/heads/main' IMG="$image" \
  sh "$root_dir/hack/verify-signed-image.sh" >/dev/null 2>&1; then
  echo 'branch signing identity unexpectedly passed release policy' >&2
  exit 1
fi
if COSIGN_FAKE_FAIL=true COSIGN_ARGS_FILE="$args_file" COSIGN_BIN="$root_dir/hack/testdata/fake-cosign.sh" \
  COSIGN_CERTIFICATE_IDENTITY="$identity" IMG="$image" \
  sh "$root_dir/hack/verify-signed-image.sh" >/dev/null 2>&1; then
  echo 'failed cosign verification unexpectedly passed policy' >&2
  exit 1
fi

unexpected_call_file="$tmp_dir/unexpected-call"
if COSIGN_FAKE_FAIL=true COSIGN_ARGS_FILE="$args_file" UNEXPECTED_CALL_FILE="$unexpected_call_file" \
  make -s -C "$root_dir" deploy-production IMG="$image" \
    COSIGN="$root_dir/hack/testdata/fake-cosign.sh" \
    COSIGN_CERTIFICATE_IDENTITY="$identity" \
    KUBECTL="$root_dir/hack/testdata/record-unexpected-call.sh" >/dev/null 2>&1; then
  echo 'production deploy unexpectedly passed failed signature verification' >&2
  exit 1
fi
if [ -e "$unexpected_call_file" ]; then
  echo 'production deploy reached Kubernetes before signature verification succeeded' >&2
  exit 1
fi

echo 'Immutable digest and exact GitHub workflow identity signature policy verified with a fake cosign boundary'
