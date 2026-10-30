#!/bin/sh
set -eu

context=${1:-kind-sentry-operator-dev}
cluster=${context#kind-}
node="$cluster-control-plane"
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin="$root_dir/.tools/bin/kind"
source_image=busybox@sha256:d82c2ab94640ded77cf76514ce6a84870761105058a4a9e51b05a8a79be97a6c
local_image=busybox:t00-arm64
image_id=sha256:d82c2ab94640ded77cf76514ce6a84870761105058a4a9e51b05a8a79be97a6c

if ! docker image inspect "$local_image" >/dev/null 2>&1; then
  docker pull "$source_image" >/dev/null
  docker tag "$source_image" "$local_image"
fi

actual_id=$(docker image inspect "$local_image" --format '{{.Id}}')
actual_arch=$(docker image inspect "$local_image" --format '{{.Architecture}}')
test "$actual_id" = "$image_id"
test "$actual_arch" = arm64

"$kind_bin" load docker-image "$local_image" --name "$cluster" >/dev/null
docker exec "$node" crictl images -o json | jq -e \
  '.images[] | select(.repoTags[]? == "docker.io/library/busybox:t00-arm64")' >/dev/null

echo "$context: probe image $local_image from $source_image loaded"
