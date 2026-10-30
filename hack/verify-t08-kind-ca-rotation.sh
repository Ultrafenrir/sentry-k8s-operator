#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
context=${1:-kind-sentry-operator-dev}
namespace=sentry-kubernetes-operator-system
source_namespace=sentry-kubernetes-operator-hook-sources
delivery_namespace=sentry-kubernetes-operator-delivery
kubectl_bin=${KUBECTL:-kubectl}
source_key=cacacacacacacacacacacacacacacacacacacacacacacacacacacacacacacaca
route_key=dadadadadadadadadadadadadadadadadadadadadadadadadadadadadadadada
event_id=t08-ca-rotation-event
source_name=sentry-hook-source-$source_key
verifier_secret=sentry-hook-receiver-sources
queue_role=sentry-hook-producer-queue
trust_secret=t08-ca-rotation-trust
producer_secret=t08-ca-rotation-producer-server
receiver_secret=t08-ca-rotation-receiver-client
public_secret=t08-ca-rotation-public-server
tmp_dir=$(mktemp -d /tmp/sentry-hook-ca-rotation.XXXXXX)
port_forward_pid=
worker_replicas=$($kubectl_bin --context "$context" -n "$namespace" get deployment telegram-delivery-worker -o jsonpath='{.spec.replicas}')
patched=false
queue_rbac_patched=false
original_queue_rules=
source_created=false
verifier_added=false
verifier_data_map_added=false
queue_name=
admission_name=

restore_queue_rbac() {
  if [ "$queue_rbac_patched" = true ] && [ -n "$original_queue_rules" ]; then
    if $kubectl_bin --context "$context" -n "$delivery_namespace" patch role "$queue_role" --type=merge \
      -p "{\"rules\":$original_queue_rules}" >/dev/null 2>&1; then
      queue_rbac_patched=false
    fi
  fi
}

case "$context" in
  kind-*) ;;
  *) echo "refusing non-kind context: $context" >&2; exit 1 ;;
esac

cleanup() {
  if [ -n "$port_forward_pid" ]; then
    kill "$port_forward_pid" >/dev/null 2>&1 || true
    wait "$port_forward_pid" >/dev/null 2>&1 || true
  fi
  restore_queue_rbac
  if [ "$patched" = true ]; then
    $kubectl_bin --context "$context" -n "$namespace" patch deployment sentry-hook-producer --type=strategic \
      -p '{"spec":{"template":{"spec":{"volumes":[{"name":"server-tls","secret":{"secretName":"sentry-hook-producer-server-tls"}},{"name":"client-ca","secret":{"secretName":"registration-ca","items":[{"key":"tls.crt","path":"tls.crt"}]}}]}}}}' >/dev/null 2>&1 || true
    $kubectl_bin --context "$context" -n "$namespace" patch deployment sentry-hook-receiver --type=strategic \
      -p '{"spec":{"template":{"spec":{"volumes":[{"name":"server-tls","secret":{"secretName":"sentry-hook-receiver-tls"}},{"name":"producer-client","secret":{"secretName":"sentry-hook-receiver-client-tls"}},{"name":"producer-ca","secret":{"secretName":"registration-ca","items":[{"key":"tls.crt","path":"tls.crt"}]}}]}}}}' >/dev/null 2>&1 || true
    $kubectl_bin --context "$context" -n "$namespace" rollout status deployment/sentry-hook-producer --timeout=180s >/dev/null 2>&1 || true
    $kubectl_bin --context "$context" -n "$namespace" rollout status deployment/sentry-hook-receiver --timeout=180s >/dev/null 2>&1 || true
  fi
  $kubectl_bin --context "$context" -n "$namespace" scale deployment/telegram-delivery-worker --replicas="$worker_replicas" >/dev/null 2>&1 || true
  if [ -n "$queue_name" ]; then
    $kubectl_bin --context "$context" -n "$delivery_namespace" delete secret "$queue_name" --ignore-not-found >/dev/null 2>&1 || true
  fi
  if [ -n "$admission_name" ]; then
    $kubectl_bin --context "$context" -n "$delivery_namespace" delete secret "$admission_name" --ignore-not-found >/dev/null 2>&1 || true
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
  $kubectl_bin --context "$context" -n "$namespace" delete secret "$trust_secret" "$producer_secret" "$receiver_secret" "$public_secret" --ignore-not-found >/dev/null 2>&1 || true
  case "$tmp_dir" in
    /tmp/sentry-hook-ca-rotation.*) rm -rf -- "$tmp_dir" ;;
  esac
}
trap cleanup EXIT INT TERM

for command in "$kubectl_bin" jq openssl curl shasum; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done
$kubectl_bin --context "$context" get nodes >/dev/null
original_queue_rules=$($kubectl_bin --context "$context" -n "$delivery_namespace" get role "$queue_role" -o json | jq -c '.rules')
test -n "$original_queue_rules"

for name in "$trust_secret" "$producer_secret" "$receiver_secret" "$public_secret"; do
  if $kubectl_bin --context "$context" -n "$namespace" get secret "$name" >/dev/null 2>&1; then
    echo "probe Secret already exists: $name" >&2
    exit 1
  fi
done
if $kubectl_bin --context "$context" -n "$source_namespace" get secret "$source_name" >/dev/null 2>&1; then
  echo "probe source already exists" >&2
  exit 1
fi
if [ -n "$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o "jsonpath={.data.${source_key}\.json}" 2>/dev/null || true)" ]; then
  echo "probe verifier entry already exists" >&2
  exit 1
fi

hook_secret=t08-ca-rotation-hmac-secret
hook_secret_base64=$(printf '%s' "$hook_secret" | openssl base64 -A)
verifier_json=$(printf '{"expectedProjectSlug":"checkout","hmacSecretsBase64":["%s"]}' "$hook_secret_base64")
verifier_base64=$(printf '%s' "$verifier_json" | openssl base64 -A)
if [ "$($kubectl_bin --context "$context" -n "$namespace" get secret "$verifier_secret" -o json | jq -r 'has("data")')" != true ]; then
  verifier_data_map_added=true
fi
$kubectl_bin --context "$context" -n "$namespace" patch secret "$verifier_secret" --type=merge \
  -p "{\"data\":{\"${source_key}.json\":\"$verifier_base64\"}}" >/dev/null
verifier_added=true

printf '%s' "{\"version\":\"v1alpha1\",\"revision\":1,\"sourceBindingKey\":\"$source_key\",\"expectedProjectSlug\":\"checkout\",\"deliveries\":[{\"routeKey\":\"$route_key\",\"render\":{\"projectName\":\"checkout\",\"alertName\":\"ca-rotation\",\"environment\":\"test\",\"severity\":\"warning\"}}]}" >"$tmp_dir/source.json"
$kubectl_bin --context "$context" -n "$source_namespace" create secret generic "$source_name" --from-file=source.json="$tmp_dir/source.json" >/dev/null
$kubectl_bin --context "$context" -n "$source_namespace" label secret "$source_name" \
  app.kubernetes.io/managed-by=sentry-kubernetes-operator sentryconfiguration.io/record-type=sentry-hook-source >/dev/null
source_created=true

event_key=$(printf '%s\0%s\0' "$source_key" "$event_id" | shasum -a 256 | awk '{print $1}')
delivery_key=$(printf '%s\0%s' "$event_key" "$route_key" | shasum -a 256 | awk '{print $1}')
queue_name=sentry-telegram-delivery-$delivery_key
admission_name=sentry-telegram-admission-$delivery_key
if $kubectl_bin --context "$context" -n "$delivery_namespace" get secret "$queue_name" >/dev/null 2>&1; then
  echo "probe queue record already exists" >&2
  exit 1
fi
$kubectl_bin --context "$context" -n "$namespace" scale deployment/telegram-delivery-worker --replicas=0 >/dev/null

copy_secret() {
  source=$1
  target=$2
  $kubectl_bin --context "$context" -n "$namespace" get secret "$source" -o json |
    jq --arg name "$target" '{apiVersion:"v1",kind:"Secret",metadata:{name:$name,namespace:.metadata.namespace},type:.type,data:.data}' |
    $kubectl_bin --context "$context" create --save-config -f - >/dev/null
}
$kubectl_bin --context "$context" -n "$namespace" get secret registration-ca -o jsonpath='{.data.tls\.crt}' |
  openssl base64 -d -A >"$tmp_dir/old-ca.crt"
$kubectl_bin --context "$context" -n "$namespace" get secret sentry-hook-receiver-tls -o jsonpath='{.data.tls\.crt}' |
  openssl base64 -d -A >"$tmp_dir/public.crt"
$kubectl_bin --context "$context" -n "$namespace" create secret generic "$trust_secret" \
  --from-file=tls.crt="$tmp_dir/old-ca.crt" --save-config >/dev/null
copy_secret sentry-hook-producer-server-tls "$producer_secret"
copy_secret sentry-hook-receiver-client-tls "$receiver_secret"
copy_secret sentry-hook-receiver-tls "$public_secret"

$kubectl_bin --context "$context" -n "$namespace" patch deployment sentry-hook-producer --type=strategic \
  -p "{\"spec\":{\"template\":{\"spec\":{\"volumes\":[{\"name\":\"server-tls\",\"secret\":{\"secretName\":\"$producer_secret\"}},{\"name\":\"client-ca\",\"secret\":{\"secretName\":\"$trust_secret\",\"items\":[{\"key\":\"tls.crt\",\"path\":\"tls.crt\"}]}}]}}}}" >/dev/null
$kubectl_bin --context "$context" -n "$namespace" patch deployment sentry-hook-receiver --type=strategic \
  -p "{\"spec\":{\"template\":{\"spec\":{\"volumes\":[{\"name\":\"server-tls\",\"secret\":{\"secretName\":\"$public_secret\"}},{\"name\":\"producer-client\",\"secret\":{\"secretName\":\"$receiver_secret\"}},{\"name\":\"producer-ca\",\"secret\":{\"secretName\":\"$trust_secret\",\"items\":[{\"key\":\"tls.crt\",\"path\":\"tls.crt\"}]}}]}}}}" >/dev/null
patched=true
$kubectl_bin --context "$context" -n "$namespace" rollout status deployment/sentry-hook-producer --timeout=180s >/dev/null
$kubectl_bin --context "$context" -n "$namespace" rollout status deployment/sentry-hook-receiver --timeout=180s >/dev/null

pod_snapshot() {
  $kubectl_bin --context "$context" -n "$namespace" get pods \
    -l 'app.kubernetes.io/component in (sentry-hook-producer,sentry-hook-receiver)' \
    -o json | jq -r '.items | sort_by(.metadata.name)[] | select(.metadata.deletionTimestamp == null and .status.containerStatuses[0].ready == true) | [.metadata.uid, (.status.containerStatuses[0].restartCount|tostring)] | @tsv'
}
before=$(pod_snapshot)
test "$(printf '%s\n' "$before" | wc -l | tr -d ' ')" = 4

server_name=sentry-hook-receiver.sentry-kubernetes-operator-system.svc
$kubectl_bin --context "$context" -n "$namespace" port-forward service/sentry-hook-receiver 18446:8446 >"$tmp_dir/port-forward.log" 2>&1 &
port_forward_pid=$!
printf '%s' "{\"project\":{\"slug\":\"checkout\"},\"event\":{\"eventID\":\"$event_id\"}}" >"$tmp_dir/body.json"
signature=$(openssl dgst -sha256 -hmac "$hook_secret" "$tmp_dir/body.json" | awk '{print $2}')

probe() {
  label=$1
  trust_file=${2:-$tmp_dir/public.crt}
  attempts=0
  while [ "$attempts" -lt 45 ]; do
    attempts=$((attempts + 1))
    timestamp=$(date +%s)
    code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --noproxy '*' \
      --connect-timeout 2 --max-time 15 --cacert "$trust_file" --resolve "$server_name:18446:127.0.0.1" \
      -H 'Content-Type: application/json' -H "X-ServiceHook-Signature: $signature" \
      -H "X-ServiceHook-Timestamp: $timestamp" -H "X-ServiceHook-GUID: $label-$attempts" \
      --data-binary "@$tmp_dir/body.json" "https://$server_name:18446/events/$source_key" 2>/dev/null || true)
    if [ "$code" = 204 ]; then
      return 0
    fi
    sleep 2
  done
  echo "$label did not reach HTTP 204" >&2
  return 1
}

restricted_queue_rules=$(printf '%s' "$original_queue_rules" | jq -c 'map(.verbs |= map(select(. != "list" and . != "delete")))')
$kubectl_bin --context "$context" -n "$delivery_namespace" patch role "$queue_role" --type=merge \
  -p "{\"rules\":$restricted_queue_rules}" >/dev/null
queue_rbac_patched=true
attempts=0
while [ "$attempts" -lt 30 ]; do
  attempts=$((attempts + 1))
  allowed=$($kubectl_bin --context "$context" auth can-i --as=system:serviceaccount:${namespace}:sentry-hook-producer \
    list secrets -n "$delivery_namespace" || true)
  [ "$allowed" = no ] && break
  sleep 1
done
test "$allowed" = no

attempts=0
code=000
while [ "$attempts" -lt 30 ]; do
  attempts=$((attempts + 1))
  timestamp=$(date +%s)
  code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --noproxy '*' \
    --connect-timeout 2 --max-time 15 --cacert "$tmp_dir/public.crt" --resolve "$server_name:18446:127.0.0.1" \
    -H 'Content-Type: application/json' -H "X-ServiceHook-Signature: $signature" \
    -H "X-ServiceHook-Timestamp: $timestamp" -H "X-ServiceHook-GUID: admission-outage-$attempts" \
    --data-binary "@$tmp_dir/body.json" "https://$server_name:18446/events/$source_key" 2>/dev/null || true)
  [ "$code" != 000 ] && break
  sleep 1
done
test "$code" = 503
$kubectl_bin --context "$context" -n "$delivery_namespace" get secret "$admission_name" -o json | jq -e \
  '.immutable == true and .metadata.labels["sentryconfiguration.io/record-type"] == "telegram-delivery-admission" and ((.data["admission.json"] | @base64d | fromjson) | .revision == 1 and .admissionPending == true)' >/dev/null
if $kubectl_bin --context "$context" -n "$delivery_namespace" get secret "$queue_name" >/dev/null 2>&1; then
  echo 'final queue record exists before admission recovery' >&2
  exit 1
fi

restore_queue_rbac
attempts=0
while [ "$attempts" -lt 30 ]; do
  attempts=$((attempts + 1))
  allowed=$($kubectl_bin --context "$context" auth can-i --as=system:serviceaccount:${namespace}:sentry-hook-producer \
    list secrets -n "$delivery_namespace" || true)
  [ "$allowed" = yes ] && break
  sleep 1
done
test "$allowed" = yes
probe admission-recovery
if $kubectl_bin --context "$context" -n "$delivery_namespace" get secret "$admission_name" >/dev/null 2>&1; then
  echo 'admission candidate remains after recovery' >&2
  exit 1
fi
probe old-ca

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp_dir/new-ca.key" >/dev/null 2>&1
openssl req -x509 -new -key "$tmp_dir/new-ca.key" -sha256 -days 1 -subj '/CN=t08-ca-rotation' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' -out "$tmp_dir/new-ca.crt"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp_dir/producer.key" >/dev/null 2>&1
openssl req -new -key "$tmp_dir/producer.key" -subj '/CN=sentry-hook-producer' \
  -addext 'subjectAltName=DNS:sentry-hook-producer.sentry-kubernetes-operator-system.svc,DNS:sentry-hook-producer.sentry-kubernetes-operator-system.svc.cluster.local' \
  -addext 'extendedKeyUsage=serverAuth' -out "$tmp_dir/producer.csr"
openssl x509 -req -in "$tmp_dir/producer.csr" -CA "$tmp_dir/new-ca.crt" -CAkey "$tmp_dir/new-ca.key" \
  -set_serial 2 -days 1 -sha256 -copy_extensions copy -out "$tmp_dir/producer.crt" >/dev/null 2>&1
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp_dir/receiver.key" >/dev/null 2>&1
openssl req -new -key "$tmp_dir/receiver.key" -subj '/CN=sentry-hook-receiver' \
  -addext 'subjectAltName=URI:spiffe://cluster.local/ns/sentry-kubernetes-operator-system/sa/sentry-hook-receiver' \
  -addext 'extendedKeyUsage=clientAuth' -out "$tmp_dir/receiver.csr"
openssl x509 -req -in "$tmp_dir/receiver.csr" -CA "$tmp_dir/new-ca.crt" -CAkey "$tmp_dir/new-ca.key" \
  -set_serial 3 -days 1 -sha256 -copy_extensions copy -out "$tmp_dir/receiver.crt" >/dev/null 2>&1
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp_dir/new-public-ca.key" >/dev/null 2>&1
openssl req -x509 -new -key "$tmp_dir/new-public-ca.key" -sha256 -days 1 -subj '/CN=t08-public-ca-rotation' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' -out "$tmp_dir/new-public-ca.crt"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp_dir/public.key" >/dev/null 2>&1
openssl req -new -key "$tmp_dir/public.key" -subj '/CN=sentry-hook-receiver' \
  -addext "subjectAltName=DNS:$server_name" -addext 'extendedKeyUsage=serverAuth' -out "$tmp_dir/public.csr"
openssl x509 -req -in "$tmp_dir/public.csr" -CA "$tmp_dir/new-public-ca.crt" -CAkey "$tmp_dir/new-public-ca.key" \
  -set_serial 4 -days 1 -sha256 -copy_extensions copy -out "$tmp_dir/public-new.crt" >/dev/null 2>&1

cp "$tmp_dir/old-ca.crt" "$tmp_dir/overlap.crt"
printf '\n' >>"$tmp_dir/overlap.crt"
openssl x509 -in "$tmp_dir/new-ca.crt" -outform PEM >>"$tmp_dir/overlap.crt"
$kubectl_bin --context "$context" -n "$namespace" create secret generic "$trust_secret" \
  --from-file=tls.crt="$tmp_dir/overlap.crt" --dry-run=client -o yaml |
  $kubectl_bin --context "$context" apply -f - >/dev/null
$kubectl_bin --context "$context" -n "$namespace" create secret tls "$producer_secret" \
  --cert="$tmp_dir/producer.crt" --key="$tmp_dir/producer.key" --dry-run=client -o yaml |
  $kubectl_bin --context "$context" apply -f - >/dev/null
$kubectl_bin --context "$context" -n "$namespace" create secret tls "$receiver_secret" \
  --cert="$tmp_dir/receiver.crt" --key="$tmp_dir/receiver.key" --dry-run=client -o yaml |
  $kubectl_bin --context "$context" apply -f - >/dev/null
probe overlap-new-leaf

cp "$tmp_dir/public.crt" "$tmp_dir/public-overlap.crt"
printf '\n' >>"$tmp_dir/public-overlap.crt"
openssl x509 -in "$tmp_dir/new-public-ca.crt" -outform PEM >>"$tmp_dir/public-overlap.crt"
$kubectl_bin --context "$context" -n "$namespace" create secret tls "$public_secret" \
  --cert="$tmp_dir/public-new.crt" --key="$tmp_dir/public.key" --dry-run=client -o yaml |
  $kubectl_bin --context "$context" apply -f - >/dev/null
probe public-overlap-new-leaf "$tmp_dir/public-overlap.crt"
if openssl s_client -connect 127.0.0.1:18446 -servername "$server_name" -CAfile "$tmp_dir/public.crt" \
  -verify_hostname "$server_name" -verify_return_error </dev/null >/dev/null 2>&1; then
  echo 'old public trust unexpectedly accepted the rotated receiver leaf' >&2
  exit 1
fi
probe public-new-ca-only "$tmp_dir/new-public-ca.crt"

$kubectl_bin --context "$context" -n "$namespace" create secret generic "$trust_secret" \
  --from-file=tls.crt="$tmp_dir/new-ca.crt" --dry-run=client -o yaml |
  $kubectl_bin --context "$context" apply -f - >/dev/null
probe new-ca-only "$tmp_dir/new-public-ca.crt"

after=$(pod_snapshot)
test "$(printf '%s\n' "$after" | wc -l | tr -d ' ')" = 4
test "$before" = "$after"
$kubectl_bin --context "$context" -n "$delivery_namespace" get secret "$queue_name" \
  -o json | jq -e '.metadata.labels["sentryconfiguration.io/delivery-state"] == "Pending" and ((.data["delivery.json"] | @base64d | fromjson) | .revision == 2 and ((.admissionPending // false) == false))' >/dev/null

echo "$context: queue RBAC outage recovered its immutable admission candidate; internal CA and public TLS cutovers reached signed ingress 204 with stable pod UIDs and zero restarts"
