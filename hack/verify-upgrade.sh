#!/bin/sh

set -eu

PREVIOUS_REF=${PREVIOUS_REF:-9567f58b6c7361f6701bbdbce60772c7d6437c32}
CLUSTER_NAME=${CLUSTER_NAME:-sko-upgrade}
KUBERNETES_IMAGE=${KUBERNETES_IMAGE:-kindest/node:v1.35.0}
SENTRY_URL=${SENTRY_URL:-http://host.docker.internal:26101}
SENTRY_HOST_URL=${SENTRY_HOST_URL:-http://127.0.0.1:26101}
SENTRY_ORGANIZATION=${SENTRY_ORGANIZATION:-sko-live-26-10-1}
SENTRY_TEAM=${SENTRY_TEAM:-sko-live}
TOKEN_FILE=${TOKEN_FILE:-.cache/sentry-self-hosted/tokens/26.10.1.token}
KEEP_UPGRADE_CLUSTER=${KEEP_UPGRADE_CLUSTER:-false}

RELEASE=sko-upgrade
OPERATOR_NAMESPACE=sko-upgrade-system
APP_NAMESPACE=sko-upgrade-fixture
PROJECT_NAME=sko-upgrade-fixture
PREVIOUS_IMAGE=sentry-kubernetes-operator:upgrade-previous
CURRENT_IMAGE=sentry-kubernetes-operator:upgrade-current
CONTEXT=kind-${CLUSTER_NAME}
TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/sko-upgrade.XXXXXX")
SUCCESS=false
ORIGINAL_CONTEXT=$(kubectl config current-context 2>/dev/null || true)

cleanup() {
	rm -rf "${TEMP_DIR}"
	if [ "${SUCCESS}" = true ] && [ "${KEEP_UPGRADE_CLUSTER}" != true ]; then
		kind delete cluster --name "${CLUSTER_NAME}" >/dev/null
	fi
	if [ -n "${ORIGINAL_CONTEXT}" ]; then
		kubectl config use-context "${ORIGINAL_CONTEXT}" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT INT TERM

for command_name in docker git helm jq kind kubectl curl; do
	command -v "${command_name}" >/dev/null 2>&1 || {
		echo "missing required command: ${command_name}" >&2
		exit 1
	}
done

test -s "${TOKEN_FILE}" || {
	echo "missing local Sentry token file: ${TOKEN_FILE}" >&2
	exit 1
}

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
	echo "kind cluster ${CLUSTER_NAME} already exists; choose another CLUSTER_NAME or remove it" >&2
	exit 1
fi

api_count() {
	token=$(cat "${TOKEN_FILE}")
	curl --fail --silent --show-error \
		-H "Authorization: Bearer ${token}" \
		"${SENTRY_HOST_URL}/api/0/organizations/${SENTRY_ORGANIZATION}/projects/" |
		jq --arg slug "${PROJECT_NAME}" '[.[] | select(.slug == $slug)] | length'
}

wait_ready() {
	resource=$1
	namespace=$2
	kubectl --context "${CONTEXT}" -n "${namespace}" wait --for=condition=Ready "${resource}" --timeout=300s
}

assert_fixture() {
	stage=$1
	wait_ready "sentryproject/${PROJECT_NAME}" "${APP_NAMESPACE}"
	remote_count=$(api_count)
	protected_count=$(kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" get secrets \
		-l sentryconfiguration.io/record-type=project-binding -o name | wc -l | tr -d ' ')
	if [ "${remote_count}" != 1 ] || [ "${protected_count}" != 1 ]; then
		echo "${stage}: expected one remote project and one protected binding; got ${remote_count}/${protected_count}" >&2
		exit 1
	fi
	kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" logs \
		"deployment/${RELEASE}-controller" --since=10m |
		if grep -Eiq 'panic|ownership lost|protected state.*(invalid|corrupt)|duplicate'; then
			echo "${stage}: unsafe controller log entry found" >&2
			exit 1
		else
			true
		fi
	echo "${stage}: Ready; remote projects=1; protected bindings=1"
}

echo "Preparing previous source ${PREVIOUS_REF}"
git archive "${PREVIOUS_REF}" | tar -x -C "${TEMP_DIR}"
docker build -t "${PREVIOUS_IMAGE}" "${TEMP_DIR}"
docker build -t "${CURRENT_IMAGE}" .

kind create cluster --name "${CLUSTER_NAME}" --image "${KUBERNETES_IMAGE}" --wait 180s
kind load docker-image --name "${CLUSTER_NAME}" "${PREVIOUS_IMAGE}" "${CURRENT_IMAGE}"

kubectl --context "${CONTEXT}" create namespace "${OPERATOR_NAMESPACE}"
kubectl --context "${CONTEXT}" create namespace "${APP_NAMESPACE}"
kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" create secret generic sentry-api-token \
	--from-file=token="${TOKEN_FILE}"
kubectl --context "${CONTEXT}" -n "${APP_NAMESPACE}" create secret generic sentry-dsns

kubectl --context "${CONTEXT}" apply -f "${TEMP_DIR}/charts/sentry-kubernetes-operator/crds"
helm --kube-context "${CONTEXT}" install "${RELEASE}" \
	"${TEMP_DIR}/charts/sentry-kubernetes-operator" \
	--namespace "${OPERATOR_NAMESPACE}" \
	--set image.repository=sentry-kubernetes-operator \
	--set image.tag=upgrade-previous \
	--set image.pullPolicy=IfNotPresent \
	--set sentryConnection.name=sko-upgrade-sentry \
	--set-string sentryConnection.spec.url="${SENTRY_URL}" \
	--set-string sentryConnection.spec.organization="${SENTRY_ORGANIZATION}" \
	--set-string sentryConnection.spec.credentials.tokenSecretRef.name=sentry-api-token \
	--set-string sentryConnection.spec.credentials.tokenSecretRef.key=token \
	--set-string sentryConnection.spec.projectManagement.unmanagedProjects=Reject \
	--set-string sentryConnection.spec.compatibility.alerts=modern-26.10.1 \
	--set sentryConnection.spec.transport.allowInsecureHTTP=true \
	--wait --timeout 5m

wait_ready sentryconnection/sko-upgrade-sentry default

cat <<EOF | kubectl --context "${CONTEXT}" apply -f -
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: ${PROJECT_NAME}
  namespace: ${APP_NAMESPACE}
spec:
  sentry: sko-upgrade-sentry
  project:
    create:
      team: ${SENTRY_TEAM}
      platform: python
      displayName: Upgrade Fixture
  dsn: {}
  settings:
    notifications:
      subjectPrefix: "[Upgrade Fixture]"
  lifecycle:
    onDelete: DeleteProject
EOF

assert_fixture previous

kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" scale \
	"deployment/${RELEASE}-controller" --replicas=0
kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" rollout status \
	"deployment/${RELEASE}-controller" --timeout=5m
kubectl --context "${CONTEXT}" apply -f charts/sentry-kubernetes-operator/crds
kubectl --context "${CONTEXT}" -n "${APP_NAMESPACE}" patch \
	"sentryproject/${PROJECT_NAME}" --type=merge \
	-p="{\"spec\":{\"team\":\"${SENTRY_TEAM}\",\"platform\":\"python\",\"displayName\":\"Upgrade Fixture\"}}"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" \
	charts/sentry-kubernetes-operator \
	--namespace "${OPERATOR_NAMESPACE}" \
	--set image.repository=sentry-kubernetes-operator \
	--set image.tag=upgrade-current \
	--set image.pullPolicy=IfNotPresent \
	--set sentryConnection.name=sko-upgrade-sentry \
	--set-string sentryConnection.spec.url="${SENTRY_URL}" \
	--set-string sentryConnection.spec.organization="${SENTRY_ORGANIZATION}" \
	--set-string sentryConnection.spec.credentials.tokenSecretRef.name=sentry-api-token \
	--set-string sentryConnection.spec.credentials.tokenSecretRef.key=token \
	--set-string sentryConnection.spec.projectManagement.unmanagedProjects=Reject \
	--set-string sentryConnection.spec.compatibility.alerts=modern-26.10.1 \
	--set sentryConnection.spec.transport.allowInsecureHTTP=true \
	--wait --timeout 5m
assert_fixture upgraded

kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" rollout restart \
	"deployment/${RELEASE}-controller"
kubectl --context "${CONTEXT}" -n "${OPERATOR_NAMESPACE}" rollout status \
	"deployment/${RELEASE}-controller" --timeout=5m
assert_fixture restarted

echo "Browser evidence URL: ${SENTRY_HOST_URL}/settings/${SENTRY_ORGANIZATION}/projects/${PROJECT_NAME}/"

kubectl --context "${CONTEXT}" -n "${APP_NAMESPACE}" delete "sentryproject/${PROJECT_NAME}" --wait=true --timeout=300s
for attempt in 1 2 3 4 5 6 7 8 9 10; do
	if [ "$(api_count)" = 0 ]; then
		break
	fi
	sleep 3
done
if [ "$(api_count)" != 0 ]; then
	echo "remote project still exists after guarded deletion" >&2
	exit 1
fi

SUCCESS=true
echo "Alpha API migration, upgrade, restart, and guarded deletion passed."
