#!/bin/sh
set -eu

context=${1:-kind-sentry-operator-dev}
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture_dir="$root_dir/testdata/kubernetes/t00-probes"
crd_file="$fixture_dir/crds/sentryprojectprobe.yaml"
namespace=sentry-operator-t00-probe
crd=sentryprojectprobes.t00.sentry-operator.dev
resource=sentryprojectprobes.t00.sentry-operator.dev

cleanup() {
  kubectl --context "$context" delete namespace "$namespace" \
    --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  kubectl --context "$context" delete crd "$crd" \
    --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
}

expect_rejected() {
  fixture=$1
  expected=$2
  if output=$(kubectl --context "$context" apply --server-side --dry-run=server \
    -f "$fixture_dir/$fixture" 2>&1); then
    echo "expected API server rejection for $fixture" >&2
    exit 1
  fi
  printf '%s\n' "$output" | rg -F "$expected" >/dev/null
}

expect_can_i() {
  expected=$1
  verb=$2
  resource_name=$3
  subresource=${4:-}
  if test -n "$subresource"; then
    actual=$(kubectl --context "$context" auth can-i "$verb" "$resource_name" \
      --subresource "$subresource" --namespace "$namespace" \
      --as "system:serviceaccount:$namespace:status-writer" || true)
  else
    actual=$(kubectl --context "$context" auth can-i "$verb" "$resource_name" \
      --namespace "$namespace" \
      --as "system:serviceaccount:$namespace:status-writer" || true)
  fi
  test "$actual" = "$expected"
}

cleanup
trap cleanup EXIT HUP INT TERM

kubectl --context "$context" apply -f "$crd_file" >/dev/null
kubectl --context "$context" wait --for=condition=Established "crd/$crd" --timeout=60s >/dev/null
kubectl --context "$context" create namespace "$namespace" >/dev/null
kubectl --context "$context" apply -f "$fixture_dir/rbac.yaml" >/dev/null

expect_can_i yes get sentryprojectprobes.t00.sentry-operator.dev
expect_can_i yes update sentryprojectprobes.t00.sentry-operator.dev status
expect_can_i no update sentryprojectprobes.t00.sentry-operator.dev
expect_can_i no delete sentryprojectprobes.t00.sentry-operator.dev
expect_can_i no get secrets

conversion=$(kubectl --context "$context" get "crd/$crd" -o jsonpath='{.spec.conversion.strategy}')
test "$conversion" = None

kubectl --context "$context" apply --server-side --dry-run=server \
  -f "$fixture_dir/valid-external.yaml" >/dev/null
kubectl --context "$context" apply -f "$fixture_dir/valid-managed.yaml" >/dev/null

managed=$(kubectl --context "$context" get "$resource/managed" -n "$namespace" -o json)
printf '%s\n' "$managed" | jq -e \
  '.spec.paused == false and .spec.lifecycle.onDelete == "Retain"' >/dev/null

generation=$(printf '%s\n' "$managed" | jq -r '.metadata.generation')
status_patch=$(jq -cn --argjson generation "$generation" \
  '{status: {observedGeneration: $generation, registrationCommit: "probe"}}')
kubectl --context "$context" patch "$resource/managed" -n "$namespace" \
  --subresource=status --type=merge -p "$status_patch" >/dev/null

updated=$(kubectl --context "$context" get "$resource/managed" -n "$namespace" -o json)
printf '%s\n' "$updated" | jq -e --argjson generation "$generation" \
  '.metadata.generation == $generation and
   .status.observedGeneration == $generation and
   .status.registrationCommit == "probe" and
   .spec.paused == false' >/dev/null

expect_rejected invalid-create-and-use.yaml \
  'exactly one of project.create or project.use is required'
expect_rejected invalid-neither-create-nor-use.yaml \
  'exactly one of project.create or project.use is required'
expect_rejected invalid-use-and-settings.yaml \
  'project.use forbids settings, destinations and alerts'
expect_rejected invalid-use-and-alerts.yaml \
  'project.use forbids settings, destinations and alerts'
expect_rejected invalid-use-and-delete.yaml \
  'project.use forbids lifecycle.onDelete DeleteProject'

kubectl --context "$context" apply -f "$fixture_dir/availability.yaml" >/dev/null
kubectl --context "$context" rollout status deployment/pdb-probe -n "$namespace" \
  --timeout=60s >/dev/null

pod=$(kubectl --context "$context" get pods -n "$namespace" \
  -l app.kubernetes.io/name=pdb-probe -o jsonpath='{.items[0].metadata.name}')
eviction=$(jq -cn --arg name "$pod" --arg namespace "$namespace" \
  '{apiVersion:"policy/v1", kind:"Eviction", metadata:{name:$name, namespace:$namespace}}')
if eviction_output=$(printf '%s\n' "$eviction" | kubectl --context "$context" create \
  --raw "/api/v1/namespaces/$namespace/pods/$pod/eviction" -f - 2>&1); then
  echo 'expected PDB to reject eviction of the only available pod' >&2
  exit 1
fi
printf '%s\n' "$eviction_output" | rg -i 'disruption budget|too many requests' >/dev/null

kubectl --context "$context" delete poddisruptionbudget pdb-probe -n "$namespace" \
  --wait=true >/dev/null
printf '%s\n' "$eviction" | kubectl --context "$context" create \
  --raw "/api/v1/namespaces/$namespace/pods/$pod/eviction" -f - >/dev/null

kubectl --context "$context" apply -f "$fixture_dir/network-connectivity.yaml" >/dev/null
kubectl --context "$context" rollout status deployment/network-server -n "$namespace" \
  --timeout=60s >/dev/null
kubectl --context "$context" wait --for=condition=Ready pod/network-client -n "$namespace" \
  --timeout=60s >/dev/null

server_ip=$(kubectl --context "$context" get pods -n "$namespace" \
  -l app.kubernetes.io/name=network-server -o jsonpath='{.items[0].status.podIP}')
connectivity=$(kubectl --context "$context" exec -n "$namespace" network-client -- \
  wget -T 2 -qO- "http://$server_ip:8080")
test "$connectivity" = ok

kubectl --context "$context" apply -f "$fixture_dir/network-policy.yaml" >/dev/null
blocked=false
attempt=0
while test "$attempt" -lt 20; do
  if kubectl --context "$context" exec -n "$namespace" network-client -- \
    wget -T 1 -qO- "http://$server_ip:8080" >/dev/null 2>&1; then
    attempt=$((attempt + 1))
    sleep 1
  else
    blocked=true
    break
  fi
done
test "$blocked" = true

echo "$context: CRD, defaults, CEL, status, RBAC, PDB, NetworkPolicy and conversion probes passed"
