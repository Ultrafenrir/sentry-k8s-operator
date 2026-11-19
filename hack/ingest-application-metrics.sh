#!/bin/sh

set -eu

NAMESPACE=${NAMESPACE:-sentry-acceptance}
PROJECT=${PROJECT:-full-modern-working}
SENTRY_ORGANIZATION=${SENTRY_ORGANIZATION:-sko-live-26-10-1}
SENTRY_HOST_URL=${SENTRY_HOST_URL:-http://127.0.0.1:26101}
SENTRY_CONTAINER=${SENTRY_CONTAINER:-sko-sentry-26-10-1-web-1}
TOKEN_FILE=${TOKEN_FILE:-.cache/sentry-self-hosted/tokens/26.10.1.token}
WAIT_SECONDS=${WAIT_SECONDS:-120}

for command_name in base64 curl docker jq kubectl; do
	command -v "${command_name}" >/dev/null 2>&1 || {
		echo "missing required command: ${command_name}" >&2
		exit 1
	}
done
test -s "${TOKEN_FILE}" || {
	echo "missing local Sentry token file: ${TOKEN_FILE}" >&2
	exit 1
}

project_id() {
	token=$(cat "${TOKEN_FILE}")
	curl --fail --silent --show-error -H "Authorization: Bearer ${token}" \
		"${SENTRY_HOST_URL}/api/0/projects/${SENTRY_ORGANIZATION}/${PROJECT}/" | jq -er '.id'
}

catalog_count() {
	token=$(cat "${TOKEN_FILE}")
	id=$(project_id)
	curl --fail --silent --show-error -H "Authorization: Bearer ${token}" \
		"${SENTRY_HOST_URL}/api/0/organizations/${SENTRY_ORGANIZATION}/trace-items/metrics/?project=${id}&statsPeriod=1h" |
		jq '[.[] | select(.name == "sko.acceptance.requests" or .name == "sko.acceptance.queue_depth" or .name == "sko.acceptance.latency")] | unique_by(.name) | length'
}

# The DSN is streamed directly from the namespace-owned output Secret into the disposable SDK
# process. It is never written to disk or printed. Localhost is translated only because the SDK
# process runs inside the Sentry Compose network.
kubectl -n "${NAMESPACE}" get secret sentry-dsn -o "jsonpath={.data.${PROJECT}}" |
	base64 -d |
	docker exec -i "${SENTRY_CONTAINER}" python -c 'import sys,urllib.parse,sentry_sdk
raw=sys.stdin.read().strip()
u=urllib.parse.urlsplit(raw)
host="host.docker.internal" if u.hostname in ("127.0.0.1","localhost") else u.hostname
netloc=(u.username+"@" if u.username else "")+host+(":"+str(u.port) if u.port else "")
dsn=urllib.parse.urlunsplit((u.scheme,netloc,u.path,u.query,u.fragment))
sentry_sdk.init(dsn=dsn,enable_metrics=True)
sentry_sdk.metrics.count("sko.acceptance.requests",7,attributes={"fixture":"operator"})
sentry_sdk.metrics.gauge("sko.acceptance.queue_depth",3,attributes={"fixture":"operator"})
sentry_sdk.metrics.distribution("sko.acceptance.latency",125,unit="millisecond",attributes={"fixture":"operator"})
sentry_sdk.flush(timeout=10)'

attempts=$((WAIT_SECONDS / 3))
attempt=1
while [ "${attempt}" -le "${attempts}" ]; do
	if [ "$(catalog_count)" = 3 ]; then
		echo "Application Metrics fixture is visible in the Sentry catalog (3/3)."
		exit 0
	fi
	attempt=$((attempt + 1))
	sleep 3
done

echo "Application Metrics fixture did not reach the Sentry catalog within ${WAIT_SECONDS}s" >&2
exit 1
