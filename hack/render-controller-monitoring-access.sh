#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
template=${CONTROLLER_MONITORING_ACCESS_TEMPLATE:-"$root_dir/config/controller-monitoring-access/cluster-role-binding.yaml"}
service_account_name=${PROMETHEUS_SERVICE_ACCOUNT_NAME:-}
service_account_namespace=${PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE:-}

require_dns_name() {
  field=$1
  value=$2
  max_length=$3
  if [ -z "$value" ] || [ "${#value}" -gt "$max_length" ]; then
    echo "$field must be a non-empty DNS name no longer than $max_length characters" >&2
    exit 1
  fi
  if ! printf '%s\n' "$value" | grep -Eq '^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$'; then
    echo "$field must be a lowercase DNS name" >&2
    exit 1
  fi
}

require_dns_name PROMETHEUS_SERVICE_ACCOUNT_NAME "$service_account_name" 253
require_dns_name PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE "$service_account_namespace" 63
case "$service_account_namespace" in
  *.*) echo 'PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE must be a DNS label without dots' >&2; exit 1 ;;
esac

sed \
  -e "s/PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE/$service_account_namespace/g" \
  -e "s/PROMETHEUS_SERVICE_ACCOUNT_NAME/$service_account_name/g" \
  "$template"
