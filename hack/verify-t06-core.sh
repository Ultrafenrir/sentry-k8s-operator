#!/bin/sh
set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
go_bin="$root_dir/.tools/go-1.26.10/bin/go"
product_gomodcache=${GOMODCACHE:-/private/tmp/sentry-operator-gomodcache}
product_gocache=${GOCACHE:-/private/tmp/sentry-operator-gocache}
envtest_assets=${KUBEBUILDER_ASSETS:-$root_dir/.tools/envtest/1.35.0}
tmp_dir=$(mktemp -d)

cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT INT TERM

cd "$root_dir"
yaml_documents() {
  ruby -ryaml -rjson -e 'YAML.load_stream(File.read(ARGV.fetch(0))).compact.each { |document| puts JSON.generate(document) }' "$1"
}

KUBEBUILDER_ASSETS="$envtest_assets" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test -race ./internal/destination ./internal/registration ./internal/telegramreceiver ./internal/kubectlplugin ./internal/resolver ./internal/binding ./internal/policy ./internal/httphealth ./internal/delivery ./internal/telegramdelivery ./internal/sentryhook
KUBEBUILDER_ASSETS="$envtest_assets" GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" test ./internal/controller ./internal/webhook/v1alpha1
GOMODCACHE="$product_gomodcache" GOCACHE="$product_gocache" \
  "$go_bin" vet ./internal/destination ./internal/registration ./internal/telegramreceiver ./internal/kubectlplugin ./internal/controller ./internal/binding ./internal/httphealth ./internal/delivery ./internal/telegramdelivery ./internal/sentryhook ./cmd/registration-writer ./cmd/telegram-receiver ./cmd/telegram-delivery-worker ./cmd/kubectl-sentry

if rg -n 'integrationIdentity|resourceIdentity|chatIdentity|topicIdentity|chatId|chatID|topicId|topicID' api/v1alpha1 config/crd/bases config/samples testdata/t01/golden; then
  echo 'private destination identity leaked into public API artifacts' >&2
  exit 1
fi

rg -q 'policy.Evaluate' internal/destination/engine.go
rg -q 'policy.EvaluateDestination' internal/destination/engine.go
rg -q 'destinationAuthorizer' internal/controller/sentryproject_controller.go
rg -q 'DestinationAccessRevoked' internal/controller/sentryproject_controller.go
rg -q 'ExternalObjectReplaced' internal/destination/engine.go
rg -q 'TelegramRegistrationRequired' internal/destination/engine.go
rg -q 'PhasePendingPublish' internal/registration/telegram.go
rg -q 'PhasePendingFenceRelease' internal/registration/telegram.go
rg -q 'c.acquireConnection' internal/registration/fences.go
rg -q 'c.acquireProject' internal/registration/fences.go
rg -q 'TokenReview' internal/registration/challenge_handler.go
rg -q 'SubjectAccessReview' internal/registration/challenge_handler.go
rg -q 'tls.RequireAndVerifyClientCert' internal/registration/internal_handler.go
rg -q 'VerifiedChains' internal/registration/internal_handler.go
rg -q 'operationSignatureHeader' internal/registration/internal_handler.go
rg -q 'ecdsa.VerifyASN1' internal/registration/internal_handler.go
rg -q 'Immutable:' internal/registration/route_store.go
rg -q 'ErrRouteConflict' internal/registration/route_store.go
rg -q 'MigrateLegacy' internal/registration/route_store.go
rg -q 'NeedLeaderElection.*true' internal/registration/route_migrator.go
rg -q 'RouteRevoked' internal/binding/types.go
rg -q 'RevokeTelegramRoute' internal/controller/sentryproject_controller.go
rg -q 'StateRestorePending' internal/destination/engine.go
rg -q 'telegram-challenge-index' internal/registration/challenge_index.go
rg -q 'SweepExpired' internal/registration/challenge_index.go
rg -q 'NeedLeaderElection.*true' internal/registration/challenge_janitor.go
rg -q 'RedeemCode' internal/registration/telegram.go
rg -q 'commits exactly one target through real Kubernetes CAS' internal/controller/registration_fence_test.go
rg -q 'resourceVersionClient' internal/registration/telegram_test.go
rg -q 'ErrAuditUnavailable' internal/registration/telegram.go
rg -q 'TestAuditFailureAfterRoutePublishKeepsResumablePendingState' internal/registration/telegram_test.go
rg -q 'audit outage mutated registration' internal/registration/fences_test.go
rg -q 'TestProjectControllerDestinationCleanupAuditFailsClosedAndRecoversDeletedRoute' internal/controller/registration_recovery_test.go
rg -q 'ComponentDestinations' internal/audit/audit.go
rg -q 'resumePendingRegistration' internal/controller/sentryproject_controller.go
rg -q 'RegistrationRecovery' cmd/main.go
rg -q 'registration.ChallengeJanitor' cmd/main.go
rg -q 'registration.RouteMigrator' cmd/main.go
rg -q 'GetConfigForClient' cmd/registration-writer/main.go
rg -q 'X-Telegram-Bot-Api-Secret-Token' internal/telegramreceiver/handler.go
rg -q 'subtle.ConstantTimeCompare' internal/telegramreceiver/handler.go
rg -q 'SignerProvider' cmd/telegram-receiver/main.go
rg -q 'GetCertificate: loadServerCertificate' cmd/telegram-receiver/main.go
rg -q 'GetClientCertificate: loadWriterCertificate' cmd/telegram-receiver/main.go
rg -q 'GetCertificate = certificateLoader' cmd/registration-writer/main.go
rg -q 'telegram-register' cmd/kubectl-sentry/main.go
rg -q 'CheckRedirect' internal/kubectlplugin/challenge.go
rg -q 'destination update is blocked while registrationCommit is active' internal/webhook/v1alpha1/sentryproject_webhook.go
rg -q 'deletion is blocked while registrationCommit is active' internal/webhook/v1alpha1/sentryconnection_webhook.go
yaml_documents config/registration/rbac.yaml | jq -s '
  any(.[]; .kind == "ClusterRole" and
    any(.rules[]; .resources == ["tokenreviews"] and .verbs == ["create"]) and
    any(.rules[]; .resources == ["subjectaccessreviews"] and .verbs == ["create"]) and
    all(.rules[]; (.resources | index("secrets")) == null)) and
  any(.[]; .kind == "Role" and .metadata.namespace == "sentry-kubernetes-operator-state" and
    any(.rules[]; .resources == ["secrets"] and (.verbs | sort) == ["create", "delete", "get", "update"])) and
  any(.[]; .kind == "Role" and .metadata.namespace == "sentry-kubernetes-operator-routing" and
    any(.rules[]; .resources == ["secrets"] and (.verbs | sort) == ["create", "delete", "get"]))
' >"$tmp_dir/registration-rbac.json"
yaml_documents config/registration/audit-rbac.yaml | jq -s -e '
  any(.[]; .kind == "Role" and .metadata.namespace == "sentry-kubernetes-operator-audit" and
    .rules == [{"apiGroups":[""],"resources":["secrets"],"verbs":["create"]}]) and
  any(.[]; .kind == "RoleBinding" and .metadata.namespace == "sentry-kubernetes-operator-audit" and
    ([.subjects[].name] | sort) == ["registration-writer","telegram-delivery-worker"])
' >"$tmp_dir/registration-audit-rbac.json"
kubectl kustomize config/registration >"$tmp_dir/registration.yaml"
yaml_documents config/registration/deployment.yaml | jq -s '
  any(.[]; .kind == "Deployment" and .spec.replicas == 2 and
    .spec.template.spec.serviceAccountName == "registration-writer" and
    .spec.template.spec.containers[0].command == ["/registration-writer"] and
    .spec.template.spec.containers[0].readinessProbe.httpGet == {"path":"/healthz","port":"challenge","scheme":"HTTPS"} and
    .spec.template.spec.containers[0].livenessProbe.httpGet == {"path":"/healthz","port":"challenge","scheme":"HTTPS"} and
    (.spec.template.spec.containers[0].args | index("--token-audience=sentry-registration") != null)) and
  any(.[]; .kind == "Deployment" and .spec.replicas == 2 and
    .spec.template.spec.serviceAccountName == "telegram-receiver" and
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].command == ["/telegram-receiver"] and
    .spec.template.spec.containers[0].readinessProbe.httpGet == {"path":"/healthz","port":"webhook","scheme":"HTTPS"} and
    .spec.template.spec.containers[0].livenessProbe.httpGet == {"path":"/healthz","port":"webhook","scheme":"HTTPS"}) and
  any(.[]; .kind == "Deployment" and .spec.replicas == 2 and
    .spec.template.spec.serviceAccountName == "telegram-delivery-worker" and
    .spec.template.spec.containers[0].command == ["/telegram-delivery-worker"] and
    .spec.template.spec.containers[0].readinessProbe.httpGet.path == "/readyz") and
  ([.[] | select(.kind == "PodDisruptionBudget" and .spec.minAvailable == 1)] | length) == 3
' >"$tmp_dir/registration-workload.json"
yaml_documents config/registration/network-policy.yaml | jq -s -e '
  any(.[]; .metadata.name == "registration-writer-ingress" and .spec.policyTypes == ["Ingress"] and
    any(.spec.ingress[]; any(.ports[]; .port == 8445) and
      .from[0].podSelector.matchLabels["app.kubernetes.io/component"] == "telegram-receiver")) and
  any(.[]; .metadata.name == "telegram-receiver" and
    (.spec.policyTypes | sort) == ["Egress", "Ingress"] and
    any(.spec.egress[]; any(.ports[]; .port == 8445)))
' >"$tmp_dir/registration-network-policy.json"
rg -q 'kind: Certificate' config/registration/certificates.yaml
rg -q 'registration-writer-internal.*svc.cluster.local' config/registration/certificates.yaml
rg -q 'COPY --from=builder /workspace/registration-writer' Dockerfile
rg -q 'COPY --from=builder /workspace/telegram-receiver' Dockerfile
rg -q 'COPY --from=builder /workspace/telegram-delivery-worker' Dockerfile
for fixture in testdata/http-fixtures/sentry-24.7.1/pagerduty-integrations-response.json testdata/http-fixtures/sentry-26.10.1/pagerduty-integrations-response.json; do
  jq -e 'length == 1 and .[0].provider.key == "pagerduty" and (.[0].configData.service_table | all(.[]; .id and .service))' "$fixture" >/dev/null
done

echo 'T06 core policy-first resolution, private pinning and revoke tombstone contracts verified (T06 remains In Progress)'
