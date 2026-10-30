#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tools_dir="$root_dir/.tools"
cache_dir="$tools_dir/cache"
bin_dir="$tools_dir/bin"
go_dir="$tools_dir/go-1.26.10"
tmp_dir=

cleanup() {
  if test -n "$tmp_dir" && test -d "$tmp_dir"; then
    rm -rf -- "$tmp_dir"
  fi
}
trap cleanup EXIT HUP INT TERM

download() {
  url=$1
  output=$2
  if test ! -f "$output"; then
    curl --fail --location --retry 3 --output "$output" "$url"
  fi
}

verify_sha256() {
  expected=$1
  file=$2
  printf '%s  %s\n' "$expected" "$file" | shasum -a 256 -c - >/dev/null
}

verify_sha512() {
  expected=$1
  file=$2
  printf '%s  %s\n' "$expected" "$file" | shasum -a 512 -c - >/dev/null
}

install_envtest() {
  version=$1
  expected=$2
  archive="$cache_dir/envtest-v$version-darwin-arm64.tar.gz"
  destination="$tools_dir/envtest/$version"
  download \
    "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v$version/envtest-v$version-darwin-arm64.tar.gz" \
    "$archive"
  verify_sha512 "$expected" "$archive"
  if test ! -x "$destination/kube-apiserver"; then
    mkdir -p "$destination"
    tar -xzf "$archive" -C "$destination" --strip-components=2
  fi
}

mkdir -p "$cache_dir" "$bin_dir" "$tools_dir/envtest"

go_archive="$cache_dir/go1.26.10.darwin-arm64.tar.gz"
download "https://go.dev/dl/go1.26.10.darwin-arm64.tar.gz" "$go_archive"
verify_sha256 0b5183492292ada170f9b561a834863808dd02edba599e4ad0e89899330020b3 "$go_archive"
if test ! -x "$go_dir/bin/go"; then
  tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sentry-operator-go.XXXXXX")
  tar -xzf "$go_archive" -C "$tmp_dir"
  mv "$tmp_dir/go" "$go_dir"
  rmdir "$tmp_dir"
  tmp_dir=
fi

kubebuilder_archive="$cache_dir/kubebuilder-v4.16.0-darwin-arm64"
download \
  "https://github.com/kubernetes-sigs/kubebuilder/releases/download/v4.16.0/kubebuilder_darwin_arm64" \
  "$kubebuilder_archive"
verify_sha256 69f5e207cf35193c78546b0e6dccfc42c44b24ceba81d3e22bab453ae118c328 \
  "$kubebuilder_archive"
install -m 0755 "$kubebuilder_archive" "$bin_dir/kubebuilder"

kind_archive="$cache_dir/kind-v0.32.0-darwin-arm64"
download \
  "https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-darwin-arm64" \
  "$kind_archive"
verify_sha256 dca67911095a110c2b5c36e26df6cac860c602033e456c0db47be498cdef1ebb \
  "$kind_archive"
install -m 0755 "$kind_archive" "$bin_dir/kind"

GOBIN="$bin_dir" GOTOOLCHAIN=local "$go_dir/bin/go" install \
  sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0

install_envtest 1.35.0 \
  bb5d0bb3975956331b0aa0c039955b4c4dc6c5c288e5af369364c7d2fbeac11a025227dabb127569080b259dd29b697cc77fa77abd27ae26721ddb23e8ee0613
install_envtest 1.36.2 \
  9278f9e5af556b2f1f2d139769c1f0d717c7b4426917fdebdba898bcb725a916e4910d6160886194ff9be9589ea7c5c32c2b8ae0754703874b7c8ba8ddfc41ce

echo "T00 toolchain installed under $tools_dir"
