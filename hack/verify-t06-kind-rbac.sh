#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
system_namespace=sentry-kubernetes-operator-system
state_namespace=sentry-kubernetes-operator-state
audit_namespace=sentry-kubernetes-operator-audit
service_account=system:serviceaccount:${system_namespace}:registration-writer
receiver_account=system:serviceaccount:${system_namespace}:telegram-receiver
worker_account=system:serviceaccount:${system_namespace}:telegram-delivery-worker

cleanup() {
  kubectl --context "$context" -n "$state_namespace" delete rolebinding/registration-protected-state role/registration-protected-state --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "$context" -n "$audit_namespace" delete rolebinding/mutation-audit-writers role/mutation-audit-writers --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "$context" delete clusterrolebinding/registration-writer clusterrole/registration-writer --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "$context" -n "$system_namespace" delete serviceaccount/registration-writer serviceaccount/telegram-receiver --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

kubectl --context "$context" create namespace "$system_namespace" --dry-run=client -o yaml | kubectl --context "$context" apply -f - >/dev/null
kubectl --context "$context" create namespace "$audit_namespace" --dry-run=client -o yaml | kubectl --context "$context" apply -f - >/dev/null
kubectl --context "$context" apply -k "$root_dir/config/crd" >/dev/null
kubectl --context "$context" apply -f "$root_dir/config/registration/rbac.yaml" >/dev/null
kubectl --context "$context" apply -f "$root_dir/config/registration/audit-rbac.yaml" >/dev/null

expect() {
  wanted=$1
  shift
  actual=$(kubectl --context "$context" auth can-i "$@" --as="$service_account" 2>/dev/null || true)
  if [ "$actual" != "$wanted" ]; then
    echo "expected '$wanted' from kubectl auth can-i $*, got '$actual'" >&2
    exit 1
  fi
}

expect yes create tokenreviews.authentication.k8s.io
expect yes create subjectaccessreviews.authorization.k8s.io
expect yes get sentryprojects.sentryconfiguration.io -n default
expect yes update sentryprojects.sentryconfiguration.io --subresource=status -n default
expect no update sentryprojects.sentryconfiguration.io -n default
expect yes get secrets -n "$state_namespace"
expect yes update secrets -n "$state_namespace"
expect yes create secrets -n "$state_namespace"
expect yes delete secrets -n "$state_namespace"
expect no get secrets -n "$system_namespace"
expect no get secrets -n default
expect yes create secrets -n "$audit_namespace"
expect no get secrets -n "$audit_namespace"
expect no list secrets -n "$audit_namespace"
expect no update secrets -n "$audit_namespace"
expect no patch secrets -n "$audit_namespace"
expect no delete secrets -n "$audit_namespace"

expect_receiver_no() {
  actual=$(kubectl --context "$context" auth can-i "$@" --as="$receiver_account" 2>/dev/null || true)
  if [ "$actual" != "no" ]; then
    echo "expected tokenless receiver to be denied by kubectl auth can-i $*, got '$actual'" >&2
    exit 1
  fi
}

expect_receiver_no get secrets -n "$state_namespace"
expect_receiver_no get sentryprojects.sentryconfiguration.io -n default
expect_receiver_no create tokenreviews.authentication.k8s.io

expect_worker() {
  wanted=$1
  shift
  actual=$(kubectl --context "$context" auth can-i "$@" --as="$worker_account" 2>/dev/null || true)
  if [ "$actual" != "$wanted" ]; then
    echo "expected '$wanted' from worker kubectl auth can-i $*, got '$actual'" >&2
    exit 1
  fi
}

expect_worker yes create secrets -n "$audit_namespace"
expect_worker no get secrets -n "$audit_namespace"
expect_worker no list secrets -n "$audit_namespace"
expect_worker no update secrets -n "$audit_namespace"
expect_worker no delete secrets -n "$audit_namespace"

echo 'T06 kind writers have create-only audit access and the public Telegram receiver has no Kubernetes permissions'
