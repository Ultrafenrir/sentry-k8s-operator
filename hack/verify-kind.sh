#!/bin/sh
set -eu

context=${1:-kind-sentry-operator-dev}

version=$(kubectl --context "$context" get --raw=/version | jq -r '.gitVersion')
test -n "$version"

not_ready_nodes=$(kubectl --context "$context" get nodes \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
  | awk '$2 != "True" { count++ } END { print count + 0 }')
test "$not_ready_nodes" -eq 0

bad_system_pods=$(kubectl --context "$context" get pods -A \
  --field-selector=status.phase!=Running,status.phase!=Succeeded -o name | wc -l | tr -d ' ')
test "$bad_system_pods" -eq 0

echo "$context: Kubernetes $version, nodes Ready, system pods healthy"
