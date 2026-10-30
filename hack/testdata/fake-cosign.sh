#!/bin/sh
set -eu

: "${COSIGN_ARGS_FILE:?COSIGN_ARGS_FILE is required}"
printf '%s\n' "$@" >"$COSIGN_ARGS_FILE"
if [ "${COSIGN_FAKE_FAIL:-false}" = true ]; then
  exit 17
fi
