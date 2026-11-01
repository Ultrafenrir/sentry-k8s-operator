#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root_dir"

required_files='
LICENSE
NOTICE
CODE_OF_CONDUCT.md
CONTRIBUTING.md
SECURITY.md
SUPPORT.md
GOVERNANCE.md
MAINTAINERS.md
CHANGELOG.md
.github/CODEOWNERS
.github/pull_request_template.md
.github/ISSUE_TEMPLATE/config.yml
.github/ISSUE_TEMPLATE/bug_report.yml
.github/ISSUE_TEMPLATE/feature_request.yml
.github/ISSUE_TEMPLATE/support_question.yml
charts/sentry-kubernetes-operator/LICENSE
charts/sentry-kubernetes-operator/NOTICE
'

for path in $required_files; do
  if [ ! -s "$path" ]; then
    echo "Required community file is missing or empty: $path" >&2
    exit 1
  fi
done

cmp LICENSE charts/sentry-kubernetes-operator/LICENSE
cmp NOTICE charts/sentry-kubernetes-operator/NOTICE

rg -q '^                                 Apache License$' LICENSE
rg -q '^                           Version 2\.0, January 2004$' LICENSE
rg -q 'Apache License 2\.0' README.md
rg -q 'artifacthub\.io/license: Apache-2\.0' charts/sentry-kubernetes-operator/Chart.yaml
rg -q '/workspace/LICENSE /licenses/sentry-kubernetes-operator/LICENSE' Dockerfile
rg -q '/workspace/NOTICE /licenses/sentry-kubernetes-operator/NOTICE' Dockerfile
rg -q '^\* @Ultrafenrir$' .github/CODEOWNERS
rg -q 'security/advisories/new' SECURITY.md .github/ISSUE_TEMPLATE/config.yml

for form in .github/ISSUE_TEMPLATE/bug_report.yml \
  .github/ISSUE_TEMPLATE/feature_request.yml \
  .github/ISSUE_TEMPLATE/support_question.yml; do
  rg -q '^name: ' "$form"
  rg -q '^description: ' "$form"
  rg -q '^body:' "$form"
done

if rg -n 'INSERT CONTACT|REPLACE ME|TODO: choose' \
  CODE_OF_CONDUCT.md CONTRIBUTING.md SECURITY.md SUPPORT.md GOVERNANCE.md \
  MAINTAINERS.md CHANGELOG.md .github; then
  echo 'Community policy contains an unresolved placeholder' >&2
  exit 1
fi

echo 'Open-source license, attribution, community policy, ownership, and GitHub templates verified'
