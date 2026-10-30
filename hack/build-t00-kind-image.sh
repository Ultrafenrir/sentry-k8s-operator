#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${1:-}
contract="$root_dir/testdata/contracts/t00-kind-images.json"

case "$version" in
  v1.35.9|v1.36.5) ;;
  *)
    echo 'usage: hack/build-t00-kind-image.sh {v1.35.9|v1.36.5}' >&2
    exit 2
    ;;
esac

kind_version=$($root_dir/.tools/bin/kind version | awk '{print $2}')
expected_kind=$(jq -r '.builder.kindVersion' "$contract")
test "$kind_version" = "$expected_kind"

base_tag=$(jq -r '.builder.baseImage' "$contract")
base_digest=$(jq -r '.builder.baseImageDigest' "$contract")
actual_base=$(docker image inspect "$base_tag" --format '{{join .RepoDigests "\n"}}')
printf '%s\n' "$actual_base" | grep -Fq "kindest/base@$base_digest"

image=$(jq -r --arg version "$version" '.images[$version].localTag' "$contract")
"$root_dir/.tools/bin/kind" build node-image "$version" \
  --type release \
  --arch arm64 \
  --base-image "$base_tag" \
  --image "$image"

echo "$image built from the pinned $version release and base image"
