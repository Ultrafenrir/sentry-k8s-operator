#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
overlay=${1:?overlay is required}
image=${2:?image is required}
kustomize_bin=${KUSTOMIZE_BIN:-"$root_dir/bin/kustomize"}
case "$kustomize_bin" in
  /*) ;;
  *) kustomize_bin="$root_dir/$kustomize_bin" ;;
esac
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

case "$overlay" in
  default)
    image_base=manager
    ;;
  registration)
    image_base=registration
    ;;
  *)
    echo "unsupported image overlay: $overlay" >&2
    exit 1
    ;;
esac

cp -R "$root_dir/config" "$tmp_dir/config"
(
  cd "$tmp_dir/config/$image_base"
  "$kustomize_bin" edit set image "controller=$image"
)
"$kustomize_bin" build "$tmp_dir/config/$overlay"
