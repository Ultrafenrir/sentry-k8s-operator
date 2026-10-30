#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tools_dir="$root_dir/.tools"

go_version=$("$tools_dir/go-1.26.10/bin/go" version)
test "$go_version" = 'go version go1.26.10 darwin/arm64'

kubebuilder_version=$("$tools_dir/bin/kubebuilder" version)
printf '%s\n' "$kubebuilder_version" | rg -F 'KubeBuilder:          v4.16.0' >/dev/null
printf '%s\n' "$kubebuilder_version" | rg -F \
  'Git Commit:           4d01fcdf146e4d832fc973ef95684a00bcf0f0c0' >/dev/null

controller_gen_version=$("$tools_dir/bin/controller-gen" --version)
printf '%s\n' "$controller_gen_version" | rg -F 'Version: v0.21.0' >/dev/null

kind_version=$("$tools_dir/bin/kind" version)
printf '%s\n' "$kind_version" | rg -F 'kind v0.32.0 ' >/dev/null

for version in 1.35.0 1.36.2; do
  assets="$tools_dir/envtest/$version"
  test -x "$assets/kube-apiserver"
  test -x "$assets/etcd"
  test -x "$assets/kubectl"
  api_version=$("$assets/kube-apiserver" --version)
  printf '%s\n' "$api_version" | rg -F "v$version" >/dev/null
done

printf '%s\n' "$go_version"
printf '%s\n' "$kubebuilder_version"
printf '%s\n' "$controller_gen_version"
printf '%s\n' "$kind_version"
echo 'envtest assets: 1.35.0 and 1.36.2 verified'
