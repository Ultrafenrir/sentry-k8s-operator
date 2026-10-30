#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin=${KIND:-$root_dir/.tools/bin/kind}
cluster_name=${T08_GATEWAY_DATAPLANE_CLUSTER:-sentry-operator-gateway-dataplane}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95}
envoy_gateway_version=${ENVOY_GATEWAY_VERSION:-v1.9.3}
envoy_proxy_image=${ENVOY_PROXY_IMAGE:-docker.io/envoyproxy/envoy:distroless-v1.39.6}
image=${IMG:-example.com/sentry-kubernetes-operator:t08-gateway-dataplane-20261030}
local_port=${GATEWAY_PROBE_PORT:-18443}
namespace=sentry-kubernetes-operator-system
tmp_dir=$(mktemp -d /tmp/sentry-operator-gateway-dataplane.XXXXXX)
created=false
port_forward_pid=

case "$cluster_name" in
  ''|*[!a-z0-9-]*) echo 'T08_GATEWAY_DATAPLANE_CLUSTER must be lowercase alphanumeric/hyphen' >&2; exit 1 ;;
esac
case "$local_port" in
  ''|*[!0-9]*) echo 'GATEWAY_PROBE_PORT must be numeric' >&2; exit 1 ;;
esac
if [ "$local_port" -lt 1024 ] || [ "$local_port" -gt 65535 ]; then
  echo 'GATEWAY_PROBE_PORT must be between 1024 and 65535' >&2
  exit 1
fi
for command in "$kind_bin" kubectl helm docker openssl curl; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done
if "$kind_bin" get clusters | grep -Fx "$cluster_name" >/dev/null 2>&1; then
  echo "refusing to reuse or delete existing kind cluster: $cluster_name" >&2
  exit 1
fi

cleanup() {
  if [ -n "$port_forward_pid" ]; then
    kill "$port_forward_pid" >/dev/null 2>&1 || true
    wait "$port_forward_pid" >/dev/null 2>&1 || true
  fi
  if [ "$created" = true ]; then
    "$kind_bin" delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
  fi
  case "$tmp_dir" in /tmp/sentry-operator-gateway-dataplane.*) rm -rf -- "$tmp_dir";; esac
}
trap cleanup EXIT INT TERM

cd "$root_dir"
make docker-build IMG="$image" >/dev/null
export KUBECONFIG="$tmp_dir/kubeconfig"
"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 120s >/dev/null
created=true
"$kind_bin" load docker-image "$image" --name "$cluster_name" >/dev/null

helm install eg oci://docker.io/envoyproxy/gateway-helm \
  --version "$envoy_gateway_version" \
  --namespace envoy-gateway-system \
  --create-namespace \
  --set "global.images.envoyProxy.image=$envoy_proxy_image" >/dev/null
kubectl -n envoy-gateway-system wait deployment/envoy-gateway --for=condition=Available --timeout=300s >/dev/null

openssl req -x509 -sha256 -nodes -days 2 -newkey rsa:2048 \
  -subj '/O=Sentry Operator Probe/CN=T08 Gateway Probe CA' \
  -keyout "$tmp_dir/ca.key" -out "$tmp_dir/ca.crt" >/dev/null 2>&1
openssl req -sha256 -nodes -newkey rsa:2048 \
  -subj '/O=Sentry Operator Probe/CN=sentry.example.com' \
  -keyout "$tmp_dir/server.key" -out "$tmp_dir/server.csr" >/dev/null 2>&1
cat >"$tmp_dir/server.ext" <<'EOF'
subjectAltName=DNS:sentry.example.com
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF
openssl x509 -req -sha256 -days 2 \
  -CA "$tmp_dir/ca.crt" -CAkey "$tmp_dir/ca.key" -set_serial 808 \
  -in "$tmp_dir/server.csr" -out "$tmp_dir/server.crt" \
  -extfile "$tmp_dir/server.ext" >/dev/null 2>&1

kubectl create namespace "$namespace" >/dev/null
kubectl -n "$namespace" create secret tls sentry-hook-receiver-tls \
  --cert="$tmp_dir/server.crt" --key="$tmp_dir/server.key" >/dev/null
kubectl -n "$namespace" create secret tls sentry-hook-receiver-client-tls \
  --cert="$tmp_dir/server.crt" --key="$tmp_dir/server.key" >/dev/null
kubectl -n "$namespace" create secret generic registration-ca \
  --from-file=tls.crt="$tmp_dir/ca.crt" >/dev/null
kubectl -n "$namespace" apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: sentry-hook-receiver-sources
type: Opaque
---
apiVersion: v1
kind: Service
metadata:
  name: telegram-receiver
spec:
  selector:
    app.kubernetes.io/name: sentry-kubernetes-operator
    app.kubernetes.io/component: sentry-hook-receiver
  ports:
  - name: webhook
    port: 443
    targetPort: 8446
EOF
sed "s#image: controller:latest#image: $image#" config/registration/hook-receiver.yaml | kubectl apply -f - >/dev/null
kubectl -n "$namespace" rollout status deployment/sentry-hook-receiver --timeout=300s >/dev/null

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: gateway-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy-t08
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: public
  namespace: gateway-system
spec:
  gatewayClassName: envoy-t08
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
EOF

GATEWAY_NAME=public \
GATEWAY_NAMESPACE=gateway-system \
GATEWAY_LISTENER=tls \
TELEGRAM_HOSTNAME=telegram.example.com \
SENTRY_HOSTNAME=sentry.example.com \
GATEWAY_WAIT_TIMEOUT=120s \
KUBECTL=kubectl \
  sh hack/manage-registration-gateway.sh deploy >/dev/null

envoy_service=
attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  envoy_service=$(kubectl -n envoy-gateway-system get service \
    -l gateway.envoyproxy.io/owning-gateway-namespace=gateway-system,gateway.envoyproxy.io/owning-gateway-name=public \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  [ -n "$envoy_service" ] && break
  sleep 1
done
test -n "$envoy_service"
envoy_deployment=$(kubectl -n envoy-gateway-system get deployment \
  -l gateway.envoyproxy.io/owning-gateway-namespace=gateway-system,gateway.envoyproxy.io/owning-gateway-name=public \
  -o jsonpath='{.items[0].metadata.name}')
test -n "$envoy_deployment"
kubectl -n envoy-gateway-system rollout status deployment/"$envoy_deployment" --timeout=300s >/dev/null

receiver_identity=$(kubectl -n "$namespace" get pods \
  -l app.kubernetes.io/component=sentry-hook-receiver \
  -o 'jsonpath={range .items[*]}{.metadata.uid}{"|"}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort)
test "$(printf '%s\n' "$receiver_identity" | wc -l | tr -d ' ')" = 2

kubectl -n envoy-gateway-system port-forward service/"$envoy_service" "$local_port:443" \
  >"$tmp_dir/port-forward.log" 2>&1 &
port_forward_pid=$!
attempt=0
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  if grep -q "Forwarding from 127.0.0.1:$local_port" "$tmp_dir/port-forward.log"; then
    break
  fi
  if ! kill -0 "$port_forward_pid" 2>/dev/null; then
    cat "$tmp_dir/port-forward.log" >&2
    exit 1
  fi
  sleep 1
done
grep -q "Forwarding from 127.0.0.1:$local_port" "$tmp_dir/port-forward.log"

curl --silent --show-error --fail \
  --noproxy '*' \
  --connect-timeout 2 --max-time 15 \
  --cacert "$tmp_dir/ca.crt" \
  --resolve "sentry.example.com:$local_port:127.0.0.1" \
  "https://sentry.example.com:$local_port/readyz" >/dev/null

current_identity=$(kubectl -n "$namespace" get pods \
  -l app.kubernetes.io/component=sentry-hook-receiver \
  -o 'jsonpath={range .items[*]}{.metadata.uid}{"|"}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | sort)
test "$current_identity" = "$receiver_identity"

echo "$cluster_name: Envoy Gateway $envoy_gateway_version accepted both TLSRoutes and passed verified SNI/TLS through to the stable sentry-hook-receiver pods"
