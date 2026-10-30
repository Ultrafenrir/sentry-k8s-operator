#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-operator-t00-admission
cluster_name=${context#kind-}
image=t00-admission-probe:local
mutating_config=t00-sentryprojectprobe-defaulting
validating_config=t00-sentryprojectprobe-validation
tmp_dir=$(mktemp -d)

cleanup() {
  kubectl --context "$context" delete mutatingwebhookconfiguration "$mutating_config" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete validatingwebhookconfiguration "$validating_config" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl --context "$context" delete crd sentryprojectprobes.t00.sentry-operator.dev --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

test "$(kubectl config get-contexts "$context" -o name)" = "$context"
node_arch=$(kubectl --context "$context" get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')
case "$node_arch" in
  arm64|amd64) ;;
  *) echo "unsupported kind node architecture: $node_arch" >&2; exit 1 ;;
esac

(
  cd "$root_dir/tools/t00-admission-probe"
  CGO_ENABLED=0 GOOS=linux GOARCH="$node_arch" GOTOOLCHAIN=local \
    GOMODCACHE="$root_dir/.tools/gomodcache" GOCACHE="$root_dir/.tools/gocache" \
    "$root_dir/.tools/go-1.26.10/bin/go" build -trimpath -o "$tmp_dir/probe" .
)
printf '%s\n' 'FROM scratch' 'COPY probe /probe' 'ENTRYPOINT ["/probe"]' >"$tmp_dir/Dockerfile"
docker build --platform "linux/$node_arch" -t "$image" "$tmp_dir" >/dev/null
"$root_dir/.tools/bin/kind" load docker-image --name "$cluster_name" "$image" >/dev/null

kubectl --context "$context" create namespace "$namespace" >/dev/null
service_dns="t00-admission.$namespace.svc"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj "/CN=$service_dns" -addext "subjectAltName=DNS:$service_dns" \
  -keyout "$tmp_dir/tls.key" -out "$tmp_dir/tls.crt" >/dev/null 2>&1
kubectl --context "$context" -n "$namespace" create secret tls t00-admission-tls \
  --cert="$tmp_dir/tls.crt" --key="$tmp_dir/tls.key" >/dev/null

kubectl --context "$context" -n "$namespace" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: t00-admission
spec:
  replicas: 1
  selector:
    matchLabels:
      app: t00-admission
  template:
    metadata:
      labels:
        app: t00-admission
    spec:
      containers:
        - name: webhook
          image: $image
          imagePullPolicy: Never
          ports:
            - name: https
              containerPort: 8443
          readinessProbe:
            httpGet:
              scheme: HTTPS
              path: /healthz
              port: https
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: t00-admission-tls
---
apiVersion: v1
kind: Service
metadata:
  name: t00-admission
spec:
  selector:
    app: t00-admission
  ports:
    - port: 443
      targetPort: https
EOF
kubectl --context "$context" -n "$namespace" rollout status deployment/t00-admission --timeout=90s >/dev/null

# A ready Pod and a completed Deployment rollout do not guarantee that the
# Service rules have propagated to the control-plane network namespace yet.
# Installing a fail-closed webhook during that short window makes the first
# admission call flaky. Exercise the same ClusterIP path as kube-apiserver
# before registering it.
cluster_ip=$(kubectl --context "$context" -n "$namespace" get service t00-admission \
  -o jsonpath='{.spec.clusterIP}')
attempt=0
while [ "$attempt" -lt 30 ]; do
  if docker exec "$cluster_name-control-plane" curl --fail --silent \
    --insecure "https://$cluster_ip:443/healthz" \
    >"$tmp_dir/healthz.out" 2>"$tmp_dir/healthz.err"; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ "$attempt" -ge 30 ]; then
  cat "$tmp_dir/healthz.err" >&2
  exit 1
fi

ca_bundle=$(base64 <"$tmp_dir/tls.crt" | tr -d '\n')
kubectl --context "$context" apply -f "$root_dir/testdata/kubernetes/t00-probes/crds/sentryprojectprobe.yaml" >/dev/null
kubectl --context "$context" apply -f - >/dev/null <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: $mutating_config
webhooks:
  - name: default.sentryprojectprobe.t00.sentry-operator.dev
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t00-admission
        path: /mutate
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["t00.sentry-operator.dev"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryprojectprobes"]
        scope: Namespaced
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: $validating_config
webhooks:
  - name: validate.sentryprojectprobe.t00.sentry-operator.dev
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t00-admission
        path: /validate
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["t00.sentry-operator.dev"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryprojectprobes"]
        scope: Namespaced
EOF

sed "s/namespace: sentry-operator-t00-probe/namespace: $namespace/" \
  "$root_dir/testdata/kubernetes/t00-probes/valid-managed.yaml" | \
  kubectl --context "$context" create -f - >/dev/null
secret_name=$(kubectl --context "$context" -n "$namespace" get sentryprojectprobe managed \
  -o jsonpath='{.spec.dsn.secret}')
test "$secret_name" = managed-sentry

if kubectl --context "$context" -n "$namespace" patch sentryprojectprobe managed \
  --type=merge -p '{"spec":{"project":{"create":{"team":"other-team"}}}}' \
  >"$tmp_dir/patch.out" 2>&1; then
  echo 'immutable spec.project update unexpectedly succeeded' >&2
  exit 1
fi
rg -q 'spec.project is immutable' "$tmp_dir/patch.out"

kubectl --context "$context" -n "$namespace" scale deployment/t00-admission --replicas=0 >/dev/null
kubectl --context "$context" -n "$namespace" wait --for=delete pod \
  -l app=t00-admission --timeout=90s >/dev/null
if sed "s/namespace: sentry-operator-t00-probe/namespace: $namespace/" \
  "$root_dir/testdata/kubernetes/t00-probes/valid-external.yaml" | \
  kubectl --context "$context" create -f - >"$tmp_dir/outage.out" 2>&1; then
  echo 'object creation unexpectedly succeeded while fail-closed webhook was unavailable' >&2
  exit 1
fi
rg -q 'failed calling webhook|no endpoints available|connection refused' "$tmp_dir/outage.out"

echo "$context: TLS admission defaulting, immutable validation and fail-closed outage passed"
