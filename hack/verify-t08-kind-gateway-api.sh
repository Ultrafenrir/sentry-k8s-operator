#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin=${KIND:-$root_dir/.tools/bin/kind}
cluster_name=${T08_GATEWAY_CLUSTER:-sentry-operator-gateway-probe}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95}
gateway_api_version=${GATEWAY_API_VERSION:-v1.5.0}
tmp_dir=$(mktemp -d /tmp/sentry-operator-gateway-probe.XXXXXX)
created=false

case "$cluster_name" in
  ''|*[!a-z0-9-]*) echo 'T08_GATEWAY_CLUSTER must be lowercase alphanumeric/hyphen' >&2; exit 1 ;;
esac
case "$gateway_api_version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo 'GATEWAY_API_VERSION must be an explicit vMAJOR.MINOR.PATCH release' >&2; exit 1 ;;
esac
for command in "$kind_bin" kubectl; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done
if "$kind_bin" get clusters | grep -Fx "$cluster_name" >/dev/null 2>&1; then
  echo "refusing to reuse or delete existing kind cluster: $cluster_name" >&2
  exit 1
fi

cleanup() {
  if [ "$created" = true ]; then
    "$kind_bin" delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
  fi
  case "$tmp_dir" in /tmp/sentry-operator-gateway-probe.*) rm -rf -- "$tmp_dir";; esac
}
trap cleanup EXIT INT TERM

export KUBECONFIG="$tmp_dir/kubeconfig"
"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 120s >/dev/null
created=true
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/$gateway_api_version/standard-install.yaml" >/dev/null

served=$(kubectl get crd tlsroutes.gateway.networking.k8s.io \
  -o 'jsonpath={.spec.versions[?(@.name=="v1")].served}{"|"}{.spec.versions[?(@.name=="v1")].storage}')
test "$served" = 'true|true'

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: gateway-system
---
apiVersion: v1
kind: Namespace
metadata:
  name: sentry-kubernetes-operator-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: public
  namespace: gateway-system
spec:
  gatewayClassName: schema-only-probe
  listeners:
  - name: tls
    hostname: "*.example.com"
    protocol: TLS
    port: 443
    tls:
      mode: Passthrough
    allowedRoutes:
      namespaces:
        from: All
---
apiVersion: v1
kind: Service
metadata:
  name: telegram-receiver
  namespace: sentry-kubernetes-operator-system
spec:
  ports:
  - name: webhook
    port: 443
    targetPort: 8443
---
apiVersion: v1
kind: Service
metadata:
  name: sentry-hook-receiver
  namespace: sentry-kubernetes-operator-system
spec:
  ports:
  - name: webhook
    port: 8446
    targetPort: 8446
EOF

GATEWAY_NAME=public \
GATEWAY_NAMESPACE=gateway-system \
GATEWAY_LISTENER=tls \
TELEGRAM_HOSTNAME=telegram.example.com \
SENTRY_HOSTNAME=sentry.example.com \
KUBECTL=kubectl \
  sh "$root_dir/hack/manage-registration-gateway.sh" verify

test "$(kubectl -n sentry-kubernetes-operator-system get tlsroute --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 0

kubectl -n gateway-system patch gateway public --type=merge \
  -p '{"spec":{"listeners":[{"name":"tls","hostname":"*.example.com","protocol":"HTTPS","port":443,"tls":{"mode":"Terminate","certificateRefs":[{"name":"missing"}]},"allowedRoutes":{"namespaces":{"from":"All"}}}]}}' >/dev/null
if GATEWAY_NAME=public \
  GATEWAY_NAMESPACE=gateway-system \
  GATEWAY_LISTENER=tls \
  TELEGRAM_HOSTNAME=telegram.example.com \
  SENTRY_HOSTNAME=sentry.example.com \
  KUBECTL=kubectl \
    sh "$root_dir/hack/manage-registration-gateway.sh" verify >"$tmp_dir/negative.log" 2>&1; then
  echo 'Gateway verifier accepted a TLS-terminating listener' >&2
  exit 1
fi
grep -q 'must be TLS/Passthrough' "$tmp_dir/negative.log"

echo "$cluster_name: Gateway API $gateway_api_version served/stored TLSRoute v1; both routes passed server dry-run and a terminating listener was rejected"
