#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root_dir"

sh -n hack/report-sentry-live-api.sh
rg -q 'sensitiveFieldCount' hack/report-sentry-live-api.sh
rg -q 'safeFieldCount' hack/report-sentry-live-api.sh
rg -q 'advancedRulesConfigured' hack/report-sentry-live-api.sh
rg -q 'detectors/' hack/report-sentry-live-api.sh
rg -q 'workflows/' hack/report-sentry-live-api.sh
rg -q 'alert-rules/' hack/report-sentry-live-api.sh
rg -q 'rules/' hack/report-sentry-live-api.sh
if rg -n 'dsn|publicKey|secretKey|integration_key|plaintext_token' hack/report-sentry-live-api.sh; then
  echo 'sensitive Sentry fields leaked into the API report projection' >&2
  exit 1
fi

echo 'Read-only redacted local Sentry API report contract verified'
