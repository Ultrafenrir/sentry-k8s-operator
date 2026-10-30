#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin=${KIND:-$root_dir/.tools/bin/kind}
cluster_name=${T09_HA_CLUSTER:-sentry-operator-ha-probe}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95}
image=${IMG:-example.com/sentry-kubernetes-operator:t09-ha-probe-20261030}
namespace=sentry-kubernetes-operator-system
deployment=sentry-kubernetes-operator-controller-manager
lease_name=c4d7095e.io
connection_name=t09-ha-missing-secret
tmp_dir=$(mktemp -d /tmp/sentry-operator-ha-probe.XXXXXX)
created=false
stopped_node=

case "$cluster_name" in
  ''|*[!a-z0-9-]*) echo 'T09_HA_CLUSTER must be lowercase alphanumeric/hyphen' >&2; exit 1 ;;
esac
for command in "$kind_bin" kubectl jq docker; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done
if "$kind_bin" get clusters | grep -Fx "$cluster_name" >/dev/null 2>&1; then
  echo "refusing to reuse or delete existing kind cluster: $cluster_name" >&2
  exit 1
fi

cleanup() {
  if [ -n "$stopped_node" ]; then
    docker start "$stopped_node" >/dev/null 2>&1 || true
  fi
  if [ "$created" = true ]; then
    "$kind_bin" delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
  fi
  case "$tmp_dir" in /tmp/sentry-operator-ha-probe.*) rm -rf -- "$tmp_dir";; esac
}
trap cleanup EXIT INT TERM

cd "$root_dir"
make docker-build IMG="$image" >/dev/null
cat >"$tmp_dir/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
- role: worker
EOF
export KUBECONFIG=$tmp_dir/kubeconfig
"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --config "$tmp_dir/kind.yaml" >/dev/null
created=true
"$kind_bin" load docker-image "$image" --name "$cluster_name" >/dev/null

kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=300s >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=300s >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s >/dev/null
make deploy IMG="$image" >/dev/null
kubectl -n "$namespace" rollout status deployment/"$deployment" --timeout=300s >/dev/null

attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  pod_json=$(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o json)
  ready_count=$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true)] | length')
  node_count=$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true) | .spec.nodeName] | unique | length')
  [ "$ready_count" = 2 ] && [ "$node_count" = 2 ] && break
  sleep 1
done
test "$ready_count" = 2
test "$node_count" = 2

kubectl apply -f - >/dev/null <<EOF
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: $connection_name
spec:
  url: https://sentry.example.invalid
  organization: example
  credentials:
    tokenSecretRef:
      name: t09-ha-missing-token
      key: token
EOF
attempt=0
condition=
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  condition=$(kubectl get sentryconnection "$connection_name" -o 'jsonpath={.status.conditions[?(@.type=="CredentialsReady")].reason}' 2>/dev/null || true)
  [ "$condition" = SecretInputInvalid ] && break
  sleep 1
done
test "$condition" = SecretInputInvalid
connection_uid=$(kubectl get sentryconnection "$connection_name" -o jsonpath='{.metadata.uid}')
test -n "$connection_uid"

holder=
attempt=0
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  holder=$(kubectl -n "$namespace" get lease "$lease_name" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [ -n "$holder" ] && break
  sleep 1
done
test -n "$holder"
leader_pod=
follower_pod=
for pod in $(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
  case "$holder" in
    "$pod"*) leader_pod=$pod ;;
    *) follower_pod=$pod ;;
  esac
done
test -n "$leader_pod"
test -n "$follower_pod"
follower_uid=$(kubectl -n "$namespace" get pod "$follower_pod" -o jsonpath='{.metadata.uid}')
follower_node=$(kubectl -n "$namespace" get pod "$follower_pod" -o jsonpath='{.spec.nodeName}')
leader_node=$(kubectl -n "$namespace" get pod "$leader_pod" -o jsonpath='{.spec.nodeName}')
test "$follower_node" != "$leader_node"

kubectl cordon "$leader_node" "$follower_node" >/dev/null
kubectl -n "$namespace" delete pod "$leader_pod" --wait=false >/dev/null
new_holder=
attempt=0
while [ "$attempt" -lt 90 ]; do
  attempt=$((attempt + 1))
  new_holder=$(kubectl -n "$namespace" get lease "$lease_name" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  case "$new_holder" in
    "$follower_pod"*) break ;;
  esac
  sleep 1
done
case "$new_holder" in
  "$follower_pod"*) ;;
  *) echo "Lease did not transfer from $leader_pod to the existing cross-node follower" >&2; exit 1 ;;
esac
kubectl uncordon "$leader_node" "$follower_node" >/dev/null
kubectl -n "$namespace" rollout status deployment/"$deployment" --timeout=300s >/dev/null

current_follower=$(kubectl -n "$namespace" get pod "$follower_pod" -o 'jsonpath={.metadata.uid}{"|"}{.status.containerStatuses[0].ready}{"|"}{.status.containerStatuses[0].restartCount}')
test "$current_follower" = "$follower_uid|true|0"
pod_json=$(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o json)
test "$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true)] | length')" = 2
test "$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true) | .spec.nodeName] | unique | length')" = 2
current_connection=$(kubectl get sentryconnection "$connection_name" -o 'jsonpath={.metadata.uid}{"|"}{.status.conditions[?(@.type=="CredentialsReady")].reason}')
test "$current_connection" = "$connection_uid|SecretInputInvalid"

kubectl create --dry-run=server -f - >/dev/null <<EOF
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: t09-ha-admission
  namespace: default
spec:
  connection: $connection_name
  project:
    create:
      team: example
EOF

holder=$(kubectl -n "$namespace" get lease "$lease_name" -o jsonpath='{.spec.holderIdentity}')
node_loss_leader=
node_loss_follower=
for pod in $(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o json | \
  jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name'); do
  case "$holder" in
    "$pod"*) node_loss_leader=$pod ;;
    *) node_loss_follower=$pod ;;
  esac
done
test -n "$node_loss_leader"
test -n "$node_loss_follower"
node_loss_node=$(kubectl -n "$namespace" get pod "$node_loss_leader" -o jsonpath='{.spec.nodeName}')
surviving_node=$(kubectl -n "$namespace" get pod "$node_loss_follower" -o jsonpath='{.spec.nodeName}')
test "$node_loss_node" != "$surviving_node"
node_loss_follower_uid=$(kubectl -n "$namespace" get pod "$node_loss_follower" -o jsonpath='{.metadata.uid}')
kubectl cordon "$node_loss_node" "$surviving_node" >/dev/null
stopped_node=$node_loss_node
docker stop "$node_loss_node" >/dev/null

node_ready=
attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  node_ready=$(kubectl get node "$node_loss_node" \
    -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  [ "$node_ready" != True ] && break
  sleep 1
done
test "$node_ready" != True

node_loss_holder=
attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  node_loss_holder=$(kubectl -n "$namespace" get lease "$lease_name" \
    -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  case "$node_loss_holder" in
    "$node_loss_follower"*) break ;;
  esac
  sleep 1
done
case "$node_loss_holder" in
  "$node_loss_follower"*) ;;
  *) echo "Lease did not transfer from node-loss leader $node_loss_leader@$node_loss_node" >&2; exit 1 ;;
esac

survivor_state=$(kubectl -n "$namespace" get pod "$node_loss_follower" \
  -o 'jsonpath={.metadata.uid}{"|"}{.status.containerStatuses[0].ready}{"|"}{.status.containerStatuses[0].restartCount}')
test "$survivor_state" = "$node_loss_follower_uid|true|0"
current_connection=$(kubectl get sentryconnection "$connection_name" \
  -o 'jsonpath={.metadata.uid}{"|"}{.status.conditions[?(@.type=="CredentialsReady")].reason}')
test "$current_connection" = "$connection_uid|SecretInputInvalid"
kubectl create --dry-run=server -f - >/dev/null <<EOF
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: t09-node-loss-admission
  namespace: default
spec:
  connection: $connection_name
  project:
    create:
      team: example
EOF

docker start "$node_loss_node" >/dev/null
stopped_node=
attempt=0
while [ "$attempt" -lt 180 ]; do
  attempt=$((attempt + 1))
  node_ready=$(kubectl get node "$node_loss_node" \
    -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  [ "$node_ready" = True ] && break
  sleep 1
done
test "$node_ready" = True
kubectl uncordon "$node_loss_node" "$surviving_node" >/dev/null
kubectl -n "$namespace" rollout status deployment/"$deployment" --timeout=300s >/dev/null

attempt=0
ready_count=0
node_count=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  pod_json=$(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o json)
  ready_count=$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true)] | length')
  node_count=$(printf '%s' "$pod_json" | jq '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true) | .spec.nodeName] | unique | length')
  [ "$ready_count" = 2 ] && [ "$node_count" = 2 ] && break
  sleep 1
done
test "$ready_count" = 2
test "$node_count" = 2
survivor_state=$(kubectl -n "$namespace" get pod "$node_loss_follower" \
  -o 'jsonpath={.metadata.uid}{"|"}{.status.containerStatuses[0].ready}{"|"}{.status.containerStatuses[0].restartCount}')
test "$survivor_state" = "$node_loss_follower_uid|true|0"
current_connection=$(kubectl get sentryconnection "$connection_name" \
  -o 'jsonpath={.metadata.uid}{"|"}{.status.conditions[?(@.type=="CredentialsReady")].reason}')
test "$current_connection" = "$connection_uid|SecretInputInvalid"

echo "$cluster_name: Lease survived pod deletion $leader_pod@$leader_node -> $follower_pod@$follower_node and worker-node loss $node_loss_leader@$node_loss_node -> $node_loss_follower@$surviving_node; CR identity/condition and admission remained available"
