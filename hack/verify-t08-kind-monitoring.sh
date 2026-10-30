#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin=${KIND:-$root_dir/.tools/bin/kind}
cluster_name=${T08_MONITORING_CLUSTER:-sentry-operator-monitoring-probe}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95}
operator_version=${PROMETHEUS_OPERATOR_VERSION:-v0.94.2}
image=${IMG:-example.com/sentry-kubernetes-operator:t08-monitoring-20261030}
local_port=${PROMETHEUS_PROBE_PORT:-19090}
namespace=sentry-kubernetes-operator-system
tmp_dir=$(mktemp -d /tmp/sentry-operator-monitoring-probe.XXXXXX)
created=false
port_forward_pid=

case "$cluster_name" in
  ''|*[!a-z0-9-]*) echo 'T08_MONITORING_CLUSTER must be lowercase alphanumeric/hyphen' >&2; exit 1 ;;
esac
case "$operator_version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo 'PROMETHEUS_OPERATOR_VERSION must be an explicit vMAJOR.MINOR.PATCH release' >&2; exit 1 ;;
esac
case "$local_port" in
  ''|*[!0-9]*) echo 'PROMETHEUS_PROBE_PORT must be numeric' >&2; exit 1 ;;
esac
if [ "$local_port" -lt 1024 ] || [ "$local_port" -gt 65535 ]; then
  echo 'PROMETHEUS_PROBE_PORT must be between 1024 and 65535' >&2
  exit 1
fi
for command in "$kind_bin" kubectl docker curl jq; do
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
  case "$tmp_dir" in /tmp/sentry-operator-monitoring-probe.*) rm -rf -- "$tmp_dir";; esac
}
trap cleanup EXIT INT TERM

cd "$root_dir"
make docker-build IMG="$image" >/dev/null
export KUBECONFIG="$tmp_dir/kubeconfig"
"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 120s >/dev/null
created=true
"$kind_bin" load docker-image "$image" --name "$cluster_name" >/dev/null

kubectl apply --server-side -f \
  "https://github.com/prometheus-operator/prometheus-operator/releases/download/$operator_version/bundle.yaml" >/dev/null
kubectl -n default wait deployment/prometheus-operator --for=condition=Available --timeout=300s >/dev/null

kubectl kustomize config/state | kubectl apply -f - >/dev/null
kubectl create namespace "$namespace" >/dev/null
kubectl -n "$namespace" create secret generic telegram-bot-auth \
  --from-literal=token='123456:monitoring-probe-not-a-real-token' >/dev/null
kubectl apply -f config/registration/delivery-rbac.yaml >/dev/null
kubectl apply -f config/registration/audit-rbac.yaml >/dev/null
sed "s#image: controller:latest#image: $image#" config/registration/delivery-worker.yaml | kubectl apply -f - >/dev/null
kubectl apply -f config/registration/delivery-network-policy.yaml >/dev/null
kubectl -n "$namespace" rollout status deployment/telegram-delivery-worker --timeout=300s >/dev/null

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: monitoring
  labels:
    metrics: enabled
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: prometheus
  namespace: monitoring
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: t08-prometheus-reader
rules:
- apiGroups: [""]
  resources: ["nodes", "nodes/metrics", "services", "endpoints", "pods", "configmaps"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["get", "list", "watch"]
- nonResourceURLs: ["/metrics"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: t08-prometheus-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: t08-prometheus-reader
subjects:
- kind: ServiceAccount
  name: prometheus
  namespace: monitoring
---
apiVersion: monitoring.coreos.com/v1
kind: Prometheus
metadata:
  name: delivery
  namespace: monitoring
spec:
  serviceAccountName: prometheus
  replicas: 1
  scrapeInterval: 5s
  evaluationInterval: 5s
  retention: 1h
  serviceMonitorSelector:
    matchLabels:
      app.kubernetes.io/name: sentry-kubernetes-operator
      app.kubernetes.io/component: telegram-delivery-worker
  serviceMonitorNamespaceSelector: {}
  ruleSelector:
    matchLabels:
      app.kubernetes.io/name: sentry-kubernetes-operator
      app.kubernetes.io/component: telegram-delivery-worker
  ruleNamespaceSelector: {}
  resources:
    requests:
      cpu: 20m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 512Mi
---
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: telegram-delivery-worker-probe
  namespace: sentry-kubernetes-operator-system
  labels:
    app.kubernetes.io/name: sentry-kubernetes-operator
    app.kubernetes.io/component: telegram-delivery-worker
spec:
  groups:
  - name: sentry-kubernetes-operator.telegram-delivery-probe
    interval: 5s
    rules:
    - alert: SentryOperatorTelegramDeliveryMonitoringProbe
      expr: sentry_operator_telegram_delivery_queue_records{state="Pending"} == 0
      for: 0s
      labels:
        severity: none
      annotations:
        summary: Disposable monitoring discovery probe
EOF
kubectl kustomize config/registration-monitoring | kubectl apply -f - >/dev/null
kubectl -n monitoring rollout status statefulset/prometheus-delivery --timeout=300s >/dev/null

kubectl -n monitoring port-forward service/prometheus-operated "$local_port:9090" \
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

api="http://127.0.0.1:$local_port"
attempt=0
series=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  response=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
    --get --data-urlencode 'query=sentry_operator_telegram_delivery_queue_records{state="Pending"}' \
    "$api/api/v1/query" 2>/dev/null || true)
  series=$(printf '%s' "$response" | jq -r '.data.result | length' 2>/dev/null || printf '0')
  [ "$series" = 2 ] && break
  sleep 1
done
test "$series" = 2
test "$(printf '%s' "$response" | jq -r '[.data.result[].value[1]] | unique == ["0"]')" = true

rules=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
  "$api/api/v1/rules?type=alert")
for alert in \
  SentryOperatorTelegramDeliveryQueueStalled \
  SentryOperatorTelegramDeliveryDeadLetters \
  SentryOperatorTelegramDeliveryMetricsRefreshFailing; do
  test "$(printf '%s' "$rules" | jq --arg alert "$alert" '[.data.groups[].rules[] | select(.name == $alert)] | length')" = 1
done

attempt=0
firing=0
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  alerts=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 "$api/api/v1/alerts")
  firing=$(printf '%s' "$alerts" | jq '[.data.alerts[] | select(.labels.alertname == "SentryOperatorTelegramDeliveryMonitoringProbe" and .state == "firing")] | length')
  [ "$firing" -ge 1 ] && break
  sleep 1
done
test "$firing" -ge 1

prometheus_identity=$(kubectl -n monitoring get pod -l app.kubernetes.io/name=prometheus \
  -o 'jsonpath={.items[0].metadata.uid}{"|"}{.items[0].status.containerStatuses[0].restartCount}')
case "$prometheus_identity" in *'|0') ;; *) echo "unexpected Prometheus identity/restarts: $prometheus_identity" >&2; exit 1;; esac

echo "$cluster_name: Prometheus Operator $operator_version discovered two worker targets, scraped identity-free queue metrics, loaded all three shipped alerts and fired the disposable product-metric probe"
