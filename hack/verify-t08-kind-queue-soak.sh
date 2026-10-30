#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
event_count=${SOAK_EVENTS:-128}
concurrency=${SOAK_CONCURRENCY:-16}
namespace=sentry-kubernetes-operator-system
source_namespace=sentry-kubernetes-operator-hook-sources
delivery_namespace=sentry-kubernetes-operator-delivery
kubectl_bin=${KUBECTL:-kubectl}
source_key=abababababababababababababababababababababababababababababababab
route_key=bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc
source_name=sentry-hook-source-$source_key
verifier_secret=sentry-hook-receiver-sources
tmp_dir=$(mktemp -d /tmp/sentry-hook-queue-soak.XXXXXX)
queue_names=$tmp_dir/queue-names
admission_names=$tmp_dir/admission-names
port_forward_pid=
source_created=false
verifier_added=false
verifier_data_map_added=false
worker_replicas=$($kubectl_bin --context "$context" -n "$namespace" get deployment telegram-delivery-worker -o jsonpath='{.spec.replicas}')

case "$context" in
  kind-*) ;;
  *) echo "refusing non-kind context: $context" >&2; exit 1 ;;
esac
case "$event_count:$concurrency" in
  *[!0-9:]*|:*|*:0|0:*) echo 'SOAK_EVENTS and SOAK_CONCURRENCY must be positive integers' >&2; exit 1 ;;
esac
if [ "$event_count" -gt 256 ] || [ "$concurrency" -gt 32 ]; then
  echo 'soak is bounded to 256 events and concurrency 32' >&2
  exit 1
fi

cleanup() {
  if [ -n "$port_forward_pid" ]; then
    kill "$port_forward_pid" >/dev/null 2>&1 || true
    wait "$port_forward_pid" >/dev/null 2>&1 || true
    port_forward_pid=
  fi
  if [ -s "$queue_names" ]; then
    xargs -n 40 "$kubectl_bin" --context "$context" -n "$delivery_namespace" delete secret --ignore-not-found <"$queue_names" >/dev/null 2>&1 || true
  fi
  if [ -s "$admission_names" ]; then
    xargs -n 40 "$kubectl_bin" --context "$context" -n "$delivery_namespace" delete secret --ignore-not-found <"$admission_names" >/dev/null 2>&1 || true
  fi
  if [ "$source_created" = true ]; then
    $kubectl_bin --context "$context" -n "$source_namespace" delete secret "$source_name" --ignore-not-found >/dev/null 2>&1 || true
  fi
  if [ "$verifier_added" = true ]; then
    current=$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o "jsonpath={.data.${source_key}\.json}" 2>/dev/null || true)
    if [ "$current" = "$verifier_base64" ]; then
      $kubectl_bin --context "$context" -n "$namespace" patch secret "$verifier_secret" --type=merge \
        -p "{\"data\":{\"${source_key}.json\":null}}" >/dev/null 2>&1 || true
    fi
  fi
  if [ "$verifier_data_map_added" = true ]; then
    remaining=$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o json 2>/dev/null | jq -r '(.data // {}) | length' 2>/dev/null || true)
    if [ "$remaining" = 0 ]; then
      $kubectl_bin --context "$context" -n "$namespace" patch secret "$verifier_secret" --type=json \
        -p '[{"op":"remove","path":"/data"}]' >/dev/null 2>&1 || true
    fi
  fi
  $kubectl_bin --context "$context" -n "$namespace" scale deployment/telegram-delivery-worker --replicas="$worker_replicas" >/dev/null 2>&1 || true
  if [ "$worker_replicas" -gt 0 ]; then
    $kubectl_bin --context "$context" -n "$namespace" rollout status deployment/telegram-delivery-worker --timeout=180s >/dev/null 2>&1 || true
  fi
}
trap 'cleanup; case "$tmp_dir" in /tmp/sentry-hook-queue-soak.*) rm -rf -- "$tmp_dir";; esac' EXIT INT TERM

for command in "$kubectl_bin" jq openssl curl shasum; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done
$kubectl_bin --context "$context" get nodes >/dev/null
test "$($kubectl_bin --context "$context" -n "$delivery_namespace" get secrets -l sentryconfiguration.io/record-type=telegram-delivery -o name | wc -l | tr -d ' ')" = 0
test "$($kubectl_bin --context "$context" -n "$delivery_namespace" get secrets -l sentryconfiguration.io/record-type=telegram-delivery-admission -o name | wc -l | tr -d ' ')" = 0
if $kubectl_bin --context "$context" -n "$source_namespace" get secret "$source_name" >/dev/null 2>&1; then
  echo 'soak source already exists' >&2
  exit 1
fi
if [ -n "$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o "jsonpath={.data.${source_key}\.json}" 2>/dev/null || true)" ]; then
  echo 'soak verifier entry already exists' >&2
  exit 1
fi

hook_secret=t08-kind-queue-soak-hmac
hook_secret_base64=$(printf '%s' "$hook_secret" | openssl base64 -A)
verifier_json=$(printf '{"expectedProjectSlug":"checkout","hmacSecretsBase64":["%s"]}' "$hook_secret_base64")
verifier_base64=$(printf '%s' "$verifier_json" | openssl base64 -A)
if [ "$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o json | jq -r 'has("data")')" != true ]; then
  verifier_data_map_added=true
fi
$kubectl_bin --context "$context" -n "$namespace" patch secret "$verifier_secret" --type=merge \
  -p "{\"data\":{\"${source_key}.json\":\"$verifier_base64\"}}" >/dev/null
verifier_added=true

printf '%s' "{\"version\":\"v1alpha1\",\"revision\":1,\"sourceBindingKey\":\"$source_key\",\"expectedProjectSlug\":\"checkout\",\"deliveries\":[{\"routeKey\":\"$route_key\",\"render\":{\"projectName\":\"checkout\",\"alertName\":\"queue-soak\",\"environment\":\"test\",\"severity\":\"warning\"}}]}" >"$tmp_dir/source.json"
$kubectl_bin --context "$context" -n "$source_namespace" create secret generic "$source_name" --from-file=source.json="$tmp_dir/source.json" >/dev/null
$kubectl_bin --context "$context" -n "$source_namespace" label secret "$source_name" \
  app.kubernetes.io/managed-by=sentry-kubernetes-operator sentryconfiguration.io/record-type=sentry-hook-source >/dev/null
source_created=true

index=0
while [ "$index" -lt "$event_count" ]; do
  event_id=$(printf 't08-soak-event-%03d' "$index")
  event_key=$(printf '%s\0%s\0' "$source_key" "$event_id" | shasum -a 256 | awk '{print $1}')
  delivery_key=$(printf '%s\0%s' "$event_key" "$route_key" | shasum -a 256 | awk '{print $1}')
  printf 'sentry-telegram-delivery-%s\n' "$delivery_key" >>"$queue_names"
  printf 'sentry-telegram-admission-%s\n' "$delivery_key" >>"$admission_names"
  index=$((index + 1))
done

$kubectl_bin --context "$context" -n "$namespace" scale deployment/telegram-delivery-worker --replicas=0 >/dev/null
$kubectl_bin --context "$context" -n "$namespace" get secret sentry-hook-receiver-tls -o jsonpath='{.data.tls\.crt}' |
  openssl base64 -d -A >"$tmp_dir/public.crt"

pod_snapshot() {
  $kubectl_bin --context "$context" -n "$namespace" get pods \
    -l 'app.kubernetes.io/component in (sentry-hook-producer,sentry-hook-receiver)' \
    -o json | jq -r '.items | sort_by(.metadata.name)[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true) | [.metadata.uid, (.status.containerStatuses[0].restartCount|tostring)] | @tsv'
}
before=$(pod_snapshot)
test "$(printf '%s\n' "$before" | wc -l | tr -d ' ')" = 4

server_name=sentry-hook-receiver.sentry-kubernetes-operator-system.svc
$kubectl_bin --context "$context" -n "$namespace" port-forward service/sentry-hook-receiver 18447:8446 >"$tmp_dir/port-forward.log" 2>&1 &
port_forward_pid=$!

send_once() {
  request_index=$1
  max_attempts=$2
  event_id=$(printf 't08-soak-event-%03d' "$request_index")
  body_file=$tmp_dir/body-$request_index.json
  printf '%s' "{\"project\":{\"slug\":\"checkout\"},\"event\":{\"eventID\":\"$event_id\"}}" >"$body_file"
  signature=$(openssl dgst -sha256 -hmac "$hook_secret" "$body_file" | awk '{print $2}')
  attempt=0
  code=000
  while [ "$attempt" -lt "$max_attempts" ] && [ "$code" != 204 ]; do
    attempt=$((attempt + 1))
    timestamp=$(date +%s)
    code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --noproxy '*' \
      --connect-timeout 2 --max-time 15 \
      --cacert "$tmp_dir/public.crt" --resolve "$server_name:18447:127.0.0.1" \
      -H 'Content-Type: application/json' -H "X-ServiceHook-Signature: $signature" \
      -H "X-ServiceHook-Timestamp: $timestamp" -H "X-ServiceHook-GUID: soak-$request_index-$attempt" \
      --data-binary "@$body_file" "https://$server_name:18447/events/$source_key" 2>/dev/null || true)
    [ "$code" = 204 ] || sleep 1
  done
  printf '%s\n' "$code" >"$tmp_dir/code-$request_index"
}

send_once 0 90
test "$(cat "$tmp_dir/code-0")" = 204
index=1
while [ "$index" -lt "$event_count" ]; do
  batch=0
  batch_pids=
  while [ "$batch" -lt "$concurrency" ] && [ "$index" -lt "$event_count" ]; do
    send_once "$index" 5 &
    batch_pids="$batch_pids $!"
    index=$((index + 1))
    batch=$((batch + 1))
  done
  for request_pid in $batch_pids; do
    wait "$request_pid"
  done
done
index=0
while [ "$index" -lt "$event_count" ]; do
  test "$(cat "$tmp_dir/code-$index")" = 204
  index=$((index + 1))
done

queue_json=$tmp_dir/queue.json
$kubectl_bin --context "$context" -n "$delivery_namespace" get secrets \
  -l sentryconfiguration.io/record-type=telegram-delivery -o json >"$queue_json"
jq -e --argjson wanted "$event_count" '
  (.items | length) == $wanted and
  all(.items[];
    .metadata.labels["sentryconfiguration.io/delivery-state"] == "Pending" and
    ((.data["delivery.json"] | @base64d | fromjson) |
      .revision == 2 and .state == "Pending" and ((.admissionPending // false) == false)))
' "$queue_json" >/dev/null
jq -r '.items[].data["delivery.json"] | @base64d' "$queue_json" >"$tmp_dir/decoded-queue.json"
test "$($kubectl_bin --context "$context" -n "$delivery_namespace" get secrets -l sentryconfiguration.io/record-type=telegram-delivery-admission -o name | wc -l | tr -d ' ')" = 0
if rg -q 't08-soak-event-|t08-kind-queue-soak-hmac' "$queue_json" "$tmp_dir/decoded-queue.json"; then
  echo 'queue leaked an external event identifier or HMAC secret' >&2
  exit 1
fi
after=$(pod_snapshot)
test "$before" = "$after"

cleanup
test "$($kubectl_bin --context "$context" -n "$delivery_namespace" get secrets -l sentryconfiguration.io/record-type=telegram-delivery -o name | wc -l | tr -d ' ')" = 0
test "$($kubectl_bin --context "$context" -n "$delivery_namespace" get secrets -l sentryconfiguration.io/record-type=telegram-delivery-admission -o name | wc -l | tr -d ' ')" = 0
trap - EXIT INT TERM
case "$tmp_dir" in /tmp/sentry-hook-queue-soak.*) rm -rf -- "$tmp_dir";; esac
echo "$context: $event_count signed unique hooks reached final Pending revision 2 at concurrency $concurrency; zero candidates, leaks and hook pod restarts"
