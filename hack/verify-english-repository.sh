#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root_dir"

set +e
matches=$(rg -n '\p{Cyrillic}' . --hidden \
  --glob '!.git/**' \
  --glob '!.cache/**' \
  --glob '!.tools/**' \
  --glob '!bin/**')
status=$?
set -e

case "$status" in
  0)
    printf '%s\n' "$matches" >&2
    echo 'Repository documentation, comments, and source files must be English-only.' >&2
    exit 1
    ;;
  1)
    echo 'Repository documentation, comments, and source files contain no Cyrillic text'
    ;;
  *)
    echo 'Failed to scan repository language policy with ripgrep' >&2
    exit "$status"
    ;;
esac
