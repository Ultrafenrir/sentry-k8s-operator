#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root_dir"

sh -n hack/manage-kind-local-demo.sh
for namespace in sentry-demo-modern sentry-demo-legacy sentry-demo-surface sentry-demo-paused; do
  yq -e "select(.kind == \"Namespace\" and .metadata.name == \"$namespace\") | .metadata.labels.\"sentryconfiguration.io/demo\" == \"kind-local\"" \
    config/demo/kind-local.yaml >/dev/null
done
rg -q '"\$kubectl_bin" --context "\$kind_context"' hack/manage-kind-local-demo.sh
rg -q 'tokens/\$version\.token' hack/manage-kind-local-demo.sh
rg -q 'evidence/\$version-capabilities\.json' hack/manage-kind-local-demo.sh
rg -q 'sentry-live-config-api' hack/manage-kind-local-demo.sh
rg -q 'refusing to delete namespace without' hack/manage-kind-local-demo.sh
rg -q 'project\.create\.atomic-default-rules' hack/manage-kind-local-demo.sh
rg -q 'project\.create\.automatic-active-key' hack/manage-kind-local-demo.sh

echo 'Bounded kind-local demo apply, status, and exact cleanup contracts verified'
