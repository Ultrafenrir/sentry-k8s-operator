#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
template=${REGISTRATION_GATEWAY_TEMPLATE:-"$root_dir/config/registration-gateway/tlsroutes.yaml"}

gateway_name=${GATEWAY_NAME:-}
gateway_namespace=${GATEWAY_NAMESPACE:-}
gateway_listener=${GATEWAY_LISTENER:-}
telegram_hostname=${TELEGRAM_HOSTNAME:-}
sentry_hostname=${SENTRY_HOSTNAME:-}

require_dns_subdomain() {
  field=$1
  value=$2
  max_length=$3
  if [ -z "$value" ] || [ "${#value}" -gt "$max_length" ]; then
    echo "$field must be a non-empty DNS name no longer than $max_length characters" >&2
    exit 1
  fi
  if ! printf '%s\n' "$value" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'; then
    echo "$field must be a lowercase DNS subdomain" >&2
    exit 1
  fi
}

require_public_hostname() {
  field=$1
  value=$2
  require_dns_subdomain "$field" "$value" 253
  case "$value" in
    *.*) ;;
    *)
      echo "$field must be an exact fully qualified DNS name" >&2
      exit 1
      ;;
  esac
}

require_dns_subdomain GATEWAY_NAME "$gateway_name" 253
require_dns_subdomain GATEWAY_NAMESPACE "$gateway_namespace" 63
require_dns_subdomain GATEWAY_LISTENER "$gateway_listener" 63
require_public_hostname TELEGRAM_HOSTNAME "$telegram_hostname"
require_public_hostname SENTRY_HOSTNAME "$sentry_hostname"

if [ "$telegram_hostname" = "$sentry_hostname" ]; then
  echo 'TELEGRAM_HOSTNAME and SENTRY_HOSTNAME must be distinct to avoid ambiguous TLSRoute backends' >&2
  exit 1
fi

sed \
  -e "s/GATEWAY_NAMESPACE/$gateway_namespace/g" \
  -e "s/GATEWAY_LISTENER/$gateway_listener/g" \
  -e "s/GATEWAY_NAME/$gateway_name/g" \
  -e "s/TELEGRAM_HOSTNAME/$telegram_hostname/g" \
  -e "s/SENTRY_HOSTNAME/$sentry_hostname/g" \
  "$template"
