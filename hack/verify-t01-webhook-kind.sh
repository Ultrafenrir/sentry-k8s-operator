#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-operator-t01-webhook
cluster_name=${context#kind-}
image=sentry-operator-t01-manager:local
mutating_config=t01-sentryconfiguration-defaulting
validating_config=t01-sentryconfiguration-validation
tmp_dir=$(mktemp -d)

cleanup() {
  kubectl --context "$context" delete mutatingwebhookconfiguration "$mutating_config" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete validatingwebhookconfiguration "$validating_config" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete clusterrolebinding t01-sentryconfiguration-manager --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete clusterrole t01-sentryconfiguration-manager --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl --context "$context" delete crd sentryprojects.sentryconfiguration.io sentryconnections.sentryconfiguration.io --ignore-not-found --wait=true >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

if kubectl --context "$context" get crd sentryprojects.sentryconfiguration.io >/dev/null 2>&1 || \
    kubectl --context "$context" get crd sentryconnections.sentryconfiguration.io >/dev/null 2>&1; then
  echo 'refusing to replace pre-existing product CRDs in the selected context' >&2
  exit 1
fi

node_arch=$(kubectl --context "$context" get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')
case "$node_arch" in
  arm64|amd64) ;;
  *) echo "unsupported kind node architecture: $node_arch" >&2; exit 1 ;;
esac

(
  cd "$root_dir"
  CGO_ENABLED=0 GOOS=linux GOARCH="$node_arch" GOTOOLCHAIN=local \
    GOMODCACHE="${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}" \
    GOCACHE="${GOCACHE:-/private/tmp/sentry-operator-gocache}" \
    "$root_dir/.tools/go-1.26.10/bin/go" build -trimpath -o "$tmp_dir/manager" ./cmd
)
printf '%s\n' \
  'FROM scratch' \
  'COPY manager /manager' \
  'USER 65532:65532' \
  'ENTRYPOINT ["/manager"]' >"$tmp_dir/Dockerfile"
docker build --platform "linux/$node_arch" -t "$image" "$tmp_dir" >/dev/null
"$root_dir/.tools/bin/kind" load docker-image --name "$cluster_name" "$image" >/dev/null

kubectl --context "$context" apply -f "$root_dir/config/crd/bases" >/dev/null
kubectl --context "$context" create namespace "$namespace" >/dev/null
service_dns="t01-webhook.$namespace.svc"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj "/CN=$service_dns" -addext "subjectAltName=DNS:$service_dns" \
  -keyout "$tmp_dir/tls.key" -out "$tmp_dir/tls.crt" >/dev/null 2>&1
kubectl --context "$context" -n "$namespace" create secret tls t01-webhook-tls \
  --cert="$tmp_dir/tls.crt" --key="$tmp_dir/tls.key" >/dev/null

kubectl --context "$context" -n "$namespace" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: t01-manager
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: t01-sentryconfiguration-manager
rules:
  - apiGroups: ["sentryconfiguration.io"]
    resources: ["sentryconnections", "sentryprojects"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["sentryconfiguration.io"]
    resources: ["sentryconnections/status", "sentryprojects/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["sentryconfiguration.io"]
    resources: ["sentryconnections/finalizers", "sentryprojects/finalizers"]
    verbs: ["update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: t01-sentryconfiguration-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: t01-sentryconfiguration-manager
subjects:
  - kind: ServiceAccount
    name: t01-manager
    namespace: $namespace
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: t01-manager
spec:
  replicas: 1
  selector:
    matchLabels:
      app: t01-manager
  template:
    metadata:
      labels:
        app: t01-manager
    spec:
      serviceAccountName: t01-manager
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: manager
          image: $image
          imagePullPolicy: Never
          args:
            - --metrics-bind-address=0
            - --health-probe-bind-address=:8081
            - --webhook-cert-path=/tls
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          ports:
            - name: webhook
              containerPort: 9443
            - name: health
              containerPort: 8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: t01-webhook-tls
---
apiVersion: v1
kind: Service
metadata:
  name: t01-webhook
spec:
  selector:
    app: t01-manager
  ports:
    - port: 443
      targetPort: webhook
EOF
kubectl --context "$context" -n "$namespace" rollout status deployment/t01-manager --timeout=120s >/dev/null

cluster_ip=$(kubectl --context "$context" -n "$namespace" get service t01-webhook -o jsonpath='{.spec.clusterIP}')
attempt=0
while [ "$attempt" -lt 30 ]; do
  if docker exec "$cluster_name-control-plane" curl --silent --insecure \
    "https://$cluster_ip:443/mutate-sentryconfiguration-io-v1alpha1-sentryproject" \
    >"$tmp_dir/webhook.out" 2>"$tmp_dir/webhook.err"; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ "$attempt" -ge 30 ]; then
  cat "$tmp_dir/webhook.err" >&2
  exit 1
fi

ca_bundle=$(base64 <"$tmp_dir/tls.crt" | tr -d '\n')
kubectl --context "$context" apply -f - >/dev/null <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: $mutating_config
webhooks:
  - name: msentryconnection-v1alpha1.t01.sentryconfiguration.io
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t01-webhook
        path: /mutate-sentryconfiguration-io-v1alpha1-sentryconnection
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["sentryconfiguration.io"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryconnections"]
  - name: msentryproject-v1alpha1.t01.sentryconfiguration.io
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t01-webhook
        path: /mutate-sentryconfiguration-io-v1alpha1-sentryproject
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["sentryconfiguration.io"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryprojects"]
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: $validating_config
webhooks:
  - name: vsentryconnection-v1alpha1.t01.sentryconfiguration.io
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t01-webhook
        path: /validate-sentryconfiguration-io-v1alpha1-sentryconnection
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["sentryconfiguration.io"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryconnections"]
  - name: vsentryproject-v1alpha1.t01.sentryconfiguration.io
    admissionReviewVersions: ["v1"]
    sideEffects: None
    failurePolicy: Fail
    clientConfig:
      caBundle: $ca_bundle
      service:
        namespace: $namespace
        name: t01-webhook
        path: /validate-sentryconfiguration-io-v1alpha1-sentryproject
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["sentryconfiguration.io"]
        apiVersions: ["v1alpha1"]
        resources: ["sentryprojects"]
EOF

sed "s/namespace: checkout-prod/namespace: $namespace/" \
  "$root_dir/testdata/t01/golden/03-external-consume-dsn.yaml" | \
  kubectl --context "$context" create -f - >/dev/null
secret_name=$(kubectl --context "$context" -n "$namespace" get sentryproject checkout-api \
  -o jsonpath='{.spec.dsn.secret}')
test "$secret_name" = checkout-api-sentry

if kubectl --context "$context" -n "$namespace" patch sentryproject checkout-api \
  --type=merge -p '{"spec":{"project":{"use":"replacement"}}}' \
  >"$tmp_dir/immutable.out" 2>&1; then
  echo 'immutable project identity update unexpectedly succeeded' >&2
  exit 1
fi
rg -q 'project identity is immutable' "$tmp_dir/immutable.out"

if kubectl --context "$context" create -f - >"$tmp_dir/policy.out" 2>&1 <<EOF
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: invalid-policy
spec:
  url: https://sentry.example.invalid
  organization: company
  credentials:
    tokenSecretRef:
      name: sentry-token
      key: token
  access:
    - owner: payments
      namespaces:
        matchNames: [$namespace]
      existingProjects:
        observe: [shared]
        consumeDSN: [unobserved]
EOF
then
  echo 'invalid consumeDSN policy unexpectedly succeeded' >&2
  exit 1
fi
rg -q 'consumeDSN must be a subset of observe' "$tmp_dir/policy.out"

kubectl --context "$context" -n "$namespace" scale deployment/t01-manager --replicas=0 >/dev/null
kubectl --context "$context" -n "$namespace" wait --for=delete pod -l app=t01-manager --timeout=90s >/dev/null
if sed "s/namespace: checkout-prod/namespace: $namespace/; s/name: checkout/name: outage/" \
  "$root_dir/testdata/t01/golden/01-managed-minimal.yaml" | \
  kubectl --context "$context" create -f - >"$tmp_dir/outage.out" 2>&1; then
  echo 'object creation unexpectedly succeeded while product webhook was unavailable' >&2
  exit 1
fi
rg -q 'failed calling webhook|no endpoints available|connection refused' "$tmp_dir/outage.out"

echo "$context: product TLS webhooks, defaults, validation and fail-closed outage passed"
