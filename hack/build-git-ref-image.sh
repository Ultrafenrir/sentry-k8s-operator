#!/bin/sh

set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <full-commit-sha> <image>" >&2
  exit 2
fi

commit_sha=$1
image=$2

case "$commit_sha" in
  *[!0-9a-f]*)
    echo "The baseline must be an exact lowercase 40-character commit SHA." >&2
    exit 2
    ;;
  *) ;;
esac
if [ "${#commit_sha}" -ne 40 ]; then
  echo "The baseline must be an exact lowercase 40-character commit SHA." >&2
  exit 2
fi

resolved=$(git rev-parse --verify "$commit_sha^{commit}")
if [ "$resolved" != "$commit_sha" ]; then
  echo "The requested baseline commit is unavailable or does not resolve exactly." >&2
  exit 1
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/sentry-operator-git-image.XXXXXX")
cleanup() {
  rm -rf "$work_dir"
}
trap cleanup EXIT HUP INT TERM

archive="$work_dir/source.tar"
source_dir="$work_dir/source"
mkdir "$source_dir"
git archive --format=tar --output="$archive" "$commit_sha"
tar -xf "$archive" -C "$source_dir"

echo "Building $image from exact baseline commit $commit_sha"
make -C "$source_dir" docker-build IMG="$image" CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
