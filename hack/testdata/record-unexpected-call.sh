#!/bin/sh
set -eu

: "${UNEXPECTED_CALL_FILE:?UNEXPECTED_CALL_FILE is required}"
printf 'called\n' >"$UNEXPECTED_CALL_FILE"
exit 99
