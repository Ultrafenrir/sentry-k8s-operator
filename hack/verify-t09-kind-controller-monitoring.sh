#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
kind_bin=${KIND:-$root_dir/.tools/bin/kind}
cluster_name=${T09_CONTROLLER_MONITORING_CLUSTER:-sentry-operator-controller-monitoring-probe}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95}
cert_manager_version=${CERT_MANAGER_VERSION:-v1.21.1}
operator_version=${PROMETHEUS_OPERATOR_VERSION:-v0.94.2}
image=${IMG:-example.com/sentry-kubernetes-operator:t09-controller-monitoring-20261030}
local_port=${PROMETHEUS_PROBE_PORT:-19091}
namespace=sentry-kubernetes-operator-system
monitoring_namespace=monitoring
service_account=prometheus
deployment=sentry-kubernetes-operator-controller-manager
metrics_service=sentry-kubernetes-operator-controller-manager-metrics-service
binding=sentry-kubernetes-operator-controller-metrics-reader
tmp_dir=$(mktemp -d /tmp/sentry-operator-controller-monitoring-probe.XXXXXX)
created=false
port_forward_pid=

case "$cluster_name" in
  ''|*[!a-z0-9-]*) echo 'T09_CONTROLLER_MONITORING_CLUSTER must be lowercase alphanumeric/hyphen' >&2; exit 1 ;;
esac
for version_name in "$cert_manager_version" "$operator_version"; do
  case "$version_name" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) echo 'dependency versions must be explicit vMAJOR.MINOR.PATCH releases' >&2; exit 1 ;;
  esac
done
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
  case "$tmp_dir" in
    /tmp/sentry-operator-controller-monitoring-probe.*) rm -rf -- "$tmp_dir" ;;
  esac
}
trap cleanup EXIT INT TERM

cd "$root_dir"
make docker-build IMG="$image" >/dev/null
export KUBECONFIG="$tmp_dir/kubeconfig"
"$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 120s >/dev/null
created=true
"$kind_bin" load docker-image "$image" --name "$cluster_name" >/dev/null

kubectl apply -f \
  "https://github.com/cert-manager/cert-manager/releases/download/$cert_manager_version/cert-manager.yaml" >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=300s >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=300s >/dev/null
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s >/dev/null

kubectl apply --server-side -f \
  "https://github.com/prometheus-operator/prometheus-operator/releases/download/$operator_version/bundle.yaml" >/dev/null
kubectl -n default wait deployment/prometheus-operator --for=condition=Available --timeout=300s >/dev/null

make deploy IMG="$image" >/dev/null
kubectl -n "$namespace" rollout status deployment/"$deployment" --timeout=300s >/dev/null

kubectl apply -f - >/dev/null <<EOF
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: t09-monitoring-missing-secret
spec:
  url: https://sentry.example.invalid
  organization: example
  credentials:
    tokenSecretRef:
      name: t09-monitoring-missing-token
      key: token
EOF
attempt=0
condition=
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  condition=$(kubectl get sentryconnection t09-monitoring-missing-secret \
    -o 'jsonpath={.status.conditions[?(@.type=="CredentialsReady")].reason}' 2>/dev/null || true)
  [ "$condition" = SecretInputInvalid ] && break
  sleep 1
done
test "$condition" = SecretInputInvalid

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
  name: t09-controller-prometheus-discovery
rules:
- apiGroups: [""]
  resources: ["services", "endpoints", "pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: t09-controller-prometheus-discovery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: t09-controller-prometheus-discovery
subjects:
- kind: ServiceAccount
  name: prometheus
  namespace: monitoring
---
apiVersion: monitoring.coreos.com/v1
kind: Prometheus
metadata:
  name: controller
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
      app.kubernetes.io/component: controller-manager
  serviceMonitorNamespaceSelector: {}
  ruleSelector:
    matchLabels:
      app.kubernetes.io/name: sentry-kubernetes-operator
      app.kubernetes.io/component: controller-manager
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
  name: controller-monitoring-probe
  namespace: sentry-kubernetes-operator-system
  labels:
    app.kubernetes.io/name: sentry-kubernetes-operator
    app.kubernetes.io/component: controller-manager
spec:
  groups:
  - name: sentry-kubernetes-operator.controller-probe
    interval: 5s
    rules:
    - alert: SentryOperatorControllerMonitoringProbe
      expr: sum(sentry_operator_reconciles_total) >= 1
      for: 0s
      labels:
        severity: none
      annotations:
        summary: Disposable authenticated monitoring probe
EOF
kubectl kustomize config/controller-monitoring | kubectl apply -f - >/dev/null
kubectl -n "$monitoring_namespace" rollout status statefulset/prometheus-controller --timeout=300s >/dev/null

pre_access=$(kubectl auth can-i get /metrics \
  --as="system:serviceaccount:$monitoring_namespace:$service_account" || true)
test "$pre_access" = no
pre_secret_access=$(kubectl auth can-i get secrets -n "$namespace" \
  --as="system:serviceaccount:$monitoring_namespace:$service_account" || true)
test "$pre_secret_access" = no

kubectl -n "$monitoring_namespace" port-forward service/prometheus-operated "$local_port:9090" \
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
target_filter='[.data.activeTargets[] | select(.labels.namespace == "sentry-kubernetes-operator-system" and .labels.service == "sentry-kubernetes-operator-controller-manager-metrics-service")]'
attempt=0
unauthorized_targets=0
while [ "$attempt" -lt 90 ]; do
  attempt=$((attempt + 1))
  targets=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
    "$api/api/v1/targets?state=active" 2>/dev/null || true)
  target_count=$(printf '%s' "$targets" | jq -r "$target_filter | length" 2>/dev/null || printf '0')
  unauthorized_targets=$(printf '%s' "$targets" | jq -r \
    "$target_filter | [.[] | select(.health == \"down\" and (.lastError | contains(\"403\")))] | length" \
    2>/dev/null || printf '0')
  [ "$target_count" = 2 ] && [ "$unauthorized_targets" = 2 ] && break
  sleep 1
done
test "$target_count" = 2
test "$unauthorized_targets" = 2

PROMETHEUS_SERVICE_ACCOUNT_NAME="$service_account" \
PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE="$monitoring_namespace" \
  sh "$root_dir/hack/manage-controller-monitoring-access.sh" deploy >/dev/null
subject=$(kubectl get clusterrolebinding "$binding" \
  -o 'jsonpath={.subjects[0].kind}{"|"}{.subjects[0].namespace}{"|"}{.subjects[0].name}{"|"}{.roleRef.name}')
test "$subject" = "ServiceAccount|$monitoring_namespace|$service_account|sentry-kubernetes-operator-metrics-reader"
test "$(kubectl auth can-i get /metrics \
  --as="system:serviceaccount:$monitoring_namespace:$service_account")" = yes
post_secret_access=$(kubectl auth can-i get secrets -n "$namespace" \
  --as="system:serviceaccount:$monitoring_namespace:$service_account" || true)
test "$post_secret_access" = no

query="up{namespace=\"$namespace\",service=\"$metrics_service\"}"
attempt=0
healthy_series=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  response=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
    --get --data-urlencode "query=$query" "$api/api/v1/query" 2>/dev/null || true)
  healthy_series=$(printf '%s' "$response" | jq -r \
    '[.data.result[] | select(.value[1] == "1")] | length' 2>/dev/null || printf '0')
  [ "$healthy_series" = 2 ] && break
  sleep 1
done
test "$healthy_series" = 2
test "$(printf '%s' "$response" | jq -r '[.data.result[].metric.instance] | unique | length')" = 2

attempt=0
product_series=0
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  product=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
    --get --data-urlencode 'query=sentry_operator_reconciles_total' "$api/api/v1/query")
  product_series=$(printf '%s' "$product" | jq -r '.data.result | length')
  [ "$product_series" -ge 1 ] && break
  sleep 1
done
test "$product_series" -ge 1

rules=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
  "$api/api/v1/rules?type=alert")
for alert in SentryOperatorReconcileErrors SentryOperatorConditionFailureBurst; do
  test "$(printf '%s' "$rules" | jq --arg alert "$alert" \
    '[.data.groups[].rules[] | select(.name == $alert)] | length')" = 1
done

attempt=0
firing=0
while [ "$attempt" -lt 60 ]; do
  attempt=$((attempt + 1))
  alerts=$(curl --silent --show-error --fail --noproxy '*' --connect-timeout 2 --max-time 10 \
    "$api/api/v1/alerts")
  firing=$(printf '%s' "$alerts" | jq \
    '[.data.alerts[] | select(.labels.alertname == "SentryOperatorControllerMonitoringProbe" and .state == "firing")] | length')
  [ "$firing" -ge 1 ] && break
  sleep 1
done
test "$firing" -ge 1

controller_state=$(kubectl -n "$namespace" get pods -l control-plane=controller-manager -o json | jq -r \
  '[.items[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true and .status.containerStatuses[0].restartCount == 0)] | length')
test "$controller_state" = 2
prometheus_identity=$(kubectl -n "$monitoring_namespace" get pod -l app.kubernetes.io/name=prometheus \
  -o 'jsonpath={.items[0].metadata.uid}{"|"}{.items[0].status.containerStatuses[0].restartCount}')
case "$prometheus_identity" in
  *'|0') ;;
  *) echo "unexpected Prometheus identity/restarts: $prometheus_identity" >&2; exit 1 ;;
esac

echo "$cluster_name: two CA/hostname-verified controller targets failed closed with 403, recovered through one exact ServiceAccount binding, exposed product metrics, loaded both shipped alerts and fired the disposable probe"
