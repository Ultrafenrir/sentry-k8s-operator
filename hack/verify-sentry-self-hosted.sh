#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
manager=$root_dir/hack/manage-sentry-self-hosted.sh
readme=$root_dir/README.md
runbook=$root_dir/docs/self-hosted-test-stack.md
makefile=$root_dir/Makefile

sh -n "$manager"

legacy=$(SENTRY_VERSION=24.7.1 SENTRY_PROFILE=feature-complete sh "$manager" metadata)
printf '%s\n' "$legacy" | grep -q '^commit=f330f3040d3ab81055f92b4ac4265fbbfe17487c$'
printf '%s\n' "$legacy" | grep -q '^project=sko-sentry-24-7-1$'
printf '%s\n' "$legacy" | grep -q '^port=24710$'

modern=$(SENTRY_VERSION=26.10.1 SENTRY_PROFILE=errors-only sh "$manager" metadata)
printf '%s\n' "$modern" | grep -q '^commit=192a826421decd7f8ce6673bb9a299737d0d6916$'
printf '%s\n' "$modern" | grep -q '^project=sko-sentry-26-10-1$'
printf '%s\n' "$modern" | grep -q '^port=26101$'

if SENTRY_VERSION=latest sh "$manager" metadata >/dev/null 2>&1; then
  echo 'unsupported floating Sentry version unexpectedly accepted' >&2
  exit 1
fi
if SENTRY_VERSION=24.7.1 SENTRY_PROFILE=invalid sh "$manager" metadata >/dev/null 2>&1; then
  echo 'invalid Sentry profile unexpectedly accepted' >&2
  exit 1
fi
if SENTRY_VERSION=24.7.1 SENTRY_PORT=80 sh "$manager" metadata >/dev/null 2>&1; then
  echo 'privileged Sentry port unexpectedly accepted' >&2
  exit 1
fi

grep -q 'reset_runtime' "$manager"
grep -q 'compose down --volumes --remove-orphans' "$manager"
grep -q 'remove_external_volumes' "$manager"
grep -q 'refusing to replace unmanaged checkout' "$manager"
grep -q 'refusing to remove unmanaged checkout' "$manager"
grep -q 'refusing concurrent Sentry versions' "$manager"
grep -q 'accept_optional_geoip_exit' "$manager"
grep -q "'geoipupdate|exited|Exited (1)'" "$manager"
grep -q 'bootstrap_access' "$manager"
grep -q "^  bootstrap)" "$manager"
grep -q "grep -Eq '\^sntryu_\[0-9a-f\]" "$manager"
grep -q 'chmod 600 "$temporary_token"' "$manager"
grep -q 'remove_managed_token' "$manager"
grep -q 'select_bash' "$manager"
grep -q 'Sentry installer requires Bash >=4.4' "$manager"
grep -q "replace_token .*'export BUILDX_BUILDER=default' 'unset BUILDX_BUILDER'" "$manager"
grep -q 'getsentry/sentry:' "$manager"
grep -q 'REPORT_SELF_HOSTED_ISSUES=0' "$manager"
grep -q 'SENTRY_EVENT_RETENTION_DAYS 1' "$manager"
grep -q 'min_feature_memory_mb=14000' "$manager"
grep -q '^verify-sentry-compose:' "$makefile"
grep -q '^sentry-matrix:' "$makefile"
grep -q '^sentry-bootstrap:' "$makefile"
grep -q 'docs/self-hosted-test-stack.md' "$readme"
grep -q 'f330f3040d3ab81055f92b4ac4265fbbfe17487c' "$runbook"
grep -q '192a826421decd7f8ce6673bb9a299737d0d6916' "$runbook"
grep -q 'never invokes Docker-wide prune' "$runbook"

echo 'Pinned Sentry 24.7.1/26.10.1 metadata, fresh-volume cleanup and managed-path safety contracts verified'
