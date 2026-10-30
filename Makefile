# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

TOOLS_DIR := $(CURDIR)/.tools
PINNED_GO := $(TOOLS_DIR)/go-1.26.10/bin/go
GO ?= $(if $(wildcard $(PINNED_GO)),$(PINNED_GO),go)
export PATH := $(TOOLS_DIR)/go-1.26.10/bin:$(PATH)
export GOTOOLCHAIN := local
export GOMODCACHE ?= /private/tmp/sentry-operator-gomodcache
export GOCACHE ?= /private/tmp/sentry-operator-gocache

KIND_CONTEXT ?= kind-sentry-operator-dev
KUBERNETES_VERSION ?=
KUBEBUILDER_ASSETS ?= $(TOOLS_DIR)/envtest/1.35.0

# Opt-in Gateway API TLS passthrough for the two public registration endpoints.
GATEWAY_NAME ?=
GATEWAY_NAMESPACE ?=
GATEWAY_LISTENER ?=
TELEGRAM_HOSTNAME ?=
SENTRY_HOSTNAME ?=
GATEWAY_WAIT_TIMEOUT ?= 120s
PROMETHEUS_SERVICE_ACCOUNT_NAME ?=
PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE ?=
SENTRY_VERSION ?= 24.7.1
SENTRY_PROFILE ?= feature-complete
SENTRY_PORT ?=
SENTRY_WAIT_TIMEOUT ?= 1800
SENTRY_SELF_HOSTED_CACHE ?= $(CURDIR)/.cache/sentry-self-hosted
SENTRY_VERSIONS ?= 24.7.1 26.10.1
SENTRY_BASH_BIN ?=

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker
BASE_IMAGE ?= docker.io/library/golang:1.26.10@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c
RUNTIME_IMAGE ?= gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: verify-sentry-self-hosted verify-sentry-compose sentry-up sentry-bootstrap sentry-live-api sentry-smoke sentry-status sentry-logs sentry-down sentry-reset sentry-clean sentry-matrix
verify-sentry-self-hosted: ## Verify pinned versions and safe fresh-volume lifecycle without starting Sentry.
	sh hack/verify-sentry-self-hosted.sh

verify-sentry-compose: ## Fetch one official pinned Sentry tree and verify its isolated Compose render.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh verify

sentry-up: ## Recreate one Sentry version from empty version-scoped volumes and wait for health.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" SENTRY_BASH_BIN="$(SENTRY_BASH_BIN)" sh hack/manage-sentry-self-hosted.sh up

sentry-bootstrap: ## Create and verify an ephemeral local API user/token without printing the token.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh bootstrap

sentry-live-api: ## Run destructive live API acceptance against the selected disposable local stack.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/verify-sentry-live-api.sh

sentry-smoke: ## Check the selected local Sentry health endpoint.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh smoke

sentry-status: ## Show only the selected version-scoped Sentry containers and volumes.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh status

sentry-logs: ## Show recent logs for the selected local Sentry stack.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh logs

sentry-down: ## Remove the selected Sentry runtime and every one of its version-scoped volumes.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh down

sentry-reset: ## Alias for sentry-down; remove the selected runtime and all managed volumes.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh reset

sentry-clean: ## Remove the selected Sentry runtime, volumes and managed upstream checkout.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh clean

sentry-matrix: ## Sequentially recreate, smoke-test and fully remove both pinned Sentry versions.
	SENTRY_VERSIONS="$(SENTRY_VERSIONS)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" SENTRY_BASH_BIN="$(SENTRY_BASH_BIN)" sh hack/verify-sentry-self-hosted-matrix.sh

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role paths=./internal/controller/... output:artifacts:config=config/rbac
	"$(CONTROLLER_GEN)" webhook paths=./internal/webhook/... output:artifacts:config=config/webhook

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths=./api/...

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test test-matrix verify-samples
test: manifests generate fmt vet ## Run tests with the pinned lower-bound envtest assets.
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

test-matrix: manifests generate fmt vet ## Run tests against both pinned envtest versions.
	KUBEBUILDER_ASSETS="$(TOOLS_DIR)/envtest/1.35.0" go test $$(go list ./... | grep -v /e2e)
	KUBEBUILDER_ASSETS="$(TOOLS_DIR)/envtest/1.36.2" go test $$(go list ./... | grep -v /e2e)

verify-samples: manifests generate fmt vet ## Validate every published sample through a real API server and admission webhooks.
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" go test ./internal/webhook/v1alpha1

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= sentry-kubernetes-operator-test-e2e
KIND_NODE_IMAGE ?= kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) --image "$(KIND_NODE_IMAGE)" ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@status=0; \
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v || status=$$?; \
	$(MAKE) cleanup-test-e2e KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) || { cleanup_status=$$?; [ $$status -ne 0 ] || status=$$cleanup_status; }; \
	exit $$status

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build build-registration-writer build-telegram-receiver build-telegram-delivery-worker build-kubectl-sentry
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

build-registration-writer: manifests generate fmt vet ## Build the separate registration writer binary.
	go build -o bin/registration-writer ./cmd/registration-writer

build-telegram-receiver: manifests generate fmt vet ## Build the public Telegram webhook receiver binary.
	go build -o bin/telegram-receiver ./cmd/telegram-receiver

build-telegram-delivery-worker: manifests generate fmt vet ## Build the isolated Telegram delivery worker binary.
	go build -o bin/telegram-delivery-worker ./cmd/telegram-delivery-worker

build-kubectl-sentry: manifests generate fmt vet ## Build the kubectl sentry Telegram registration plugin.
	go build -o bin/kubectl-sentry ./cmd/kubectl-sentry

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Development builds may override BASE_IMAGE or RUNTIME_IMAGE explicitly. Release builds
# require both overrides to remain immutable repository@sha256 references.
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --build-arg BASE_IMAGE="$(BASE_IMAGE)" --build-arg RUNTIME_IMAGE="$(RUNTIME_IMAGE)" -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
BUILDX_BUILDER ?= sentry-kubernetes-operator-builder
RELEASE_METADATA ?= dist/build-metadata.json
COSIGN ?= cosign
COSIGN_CERTIFICATE_IDENTITY ?=
.PHONY: docker-buildx verify-release-tag verify-release-metadata verify-image-digest verify-signed-image verify-signature-policy verify-release-workflow verify-base-image-digests
verify-release-tag: ## Reject an unversioned or latest tag for a pushed release image.
	@[[ "$(IMG)" =~ ^[^[:space:]@]+:[^[:space:]@]+$$ && "$(IMG)" != *:latest ]] || { echo 'IMG must be an explicit non-latest registry tag' >&2; exit 1; }

verify-base-image-digests: ## Require immutable builder and runtime images for a release build.
	@[[ "$(BASE_IMAGE)" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$$ ]] || { echo 'BASE_IMAGE must be repository@sha256:<64 lowercase hex>' >&2; exit 1; }
	@[[ "$(RUNTIME_IMAGE)" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$$ ]] || { echo 'RUNTIME_IMAGE must be repository@sha256:<64 lowercase hex>' >&2; exit 1; }

docker-buildx: verify-release-tag verify-base-image-digests ## Build/push multi-platform image with SBOM and max provenance attestations.
	@$(CONTAINER_TOOL) buildx inspect "$(BUILDX_BUILDER)" >/dev/null 2>&1 || $(CONTAINER_TOOL) buildx create --name "$(BUILDX_BUILDER)" >/dev/null
	mkdir -p "$(dir $(RELEASE_METADATA))"
	BUILDX_METADATA_PROVENANCE=max $(CONTAINER_TOOL) buildx build --builder "$(BUILDX_BUILDER)" --push --platform=$(PLATFORMS) --sbom=true --provenance=mode=max --metadata-file "$(RELEASE_METADATA)" --build-arg BASE_IMAGE="$(BASE_IMAGE)" --build-arg RUNTIME_IMAGE="$(RUNTIME_IMAGE)" --tag "$(IMG)" .
	$(MAKE) verify-release-metadata RELEASE_METADATA="$(RELEASE_METADATA)"

verify-release-metadata: ## Verify that Buildx recorded one canonical pushed image digest.
	@jq -e '.["containerimage.digest"] | strings | test("^sha256:[0-9a-f]{64}$$")' "$(RELEASE_METADATA)" >/dev/null

verify-image-digest: ## Require immutable repository@sha256 identity for production deployment.
	@[[ "$(IMG)" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$$ ]] || { echo 'IMG must be repository@sha256:<64 lowercase hex> for production deployment' >&2; exit 1; }

verify-signed-image: verify-image-digest ## Verify the exact digest against one pinned GitHub release-workflow identity.
	COSIGN_BIN="$(COSIGN)" COSIGN_CERTIFICATE_IDENTITY="$(COSIGN_CERTIFICATE_IDENTITY)" IMG="$(IMG)" sh hack/verify-signed-image.sh

verify-signature-policy: ## Source-test immutable digest, exact identity and verification-failure gates without a registry.
	sh hack/verify-signature-policy.sh

verify-release-workflow: ## Verify pinned tag-only keyless signing workflow structure.
	sh hack/verify-release-workflow.sh

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	KUSTOMIZE_BIN="$(KUSTOMIZE)" sh hack/render-kustomize-image.sh default "$(IMG)" > dist/install.yaml

.PHONY: helm-generate verify-helm helm-package
helm-generate: manifests generate ## Regenerate chart CRDs and manifest payloads from canonical Kustomize sources.
	sh hack/update-helm-chart.sh

verify-helm: manifests generate ## Verify chart drift, schema, supported Kubernetes versions and opt-in rendering.
	sh hack/verify-helm.sh

helm-package: verify-helm ## Package the verified alpha chart into dist/.
	mkdir -p dist
	helm package charts/sentry-kubernetes-operator --destination dist

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy deploy-production deploy-registration deploy-registration-production deploy-registration-gateway deploy-registration-monitoring deploy-controller-monitoring deploy-controller-monitoring-access verify-registration-prerequisites verify-registration-gateway-prerequisites verify-registration-monitoring-prerequisites verify-controller-monitoring-prerequisites verify-controller-monitoring-access-prerequisites
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/state | "$(KUBECTL)" apply -f -
	KUSTOMIZE_BIN="$(KUSTOMIZE)" sh hack/render-kustomize-image.sh default "$(IMG)" | "$(KUBECTL)" apply -f -

deploy-production: verify-signed-image ## Deploy controller only from a verified signed immutable image digest.
	$(MAKE) deploy IMG="$(IMG)"

verify-registration-prerequisites: ## Verify cert-manager and administrator-provided Telegram/Sentry ingress Secrets.
	"$(KUBECTL)" get crd certificates.cert-manager.io >/dev/null
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret telegram-webhook-tls -o jsonpath='{.data.tls\.crt}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret telegram-webhook-tls -o jsonpath='{.data.tls\.key}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret telegram-webhook-auth -o jsonpath='{.data.secret-token}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret telegram-bot-auth -o jsonpath='{.data.token}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret sentry-hook-receiver-tls -o jsonpath='{.data.tls\.crt}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret sentry-hook-receiver-tls -o jsonpath='{.data.tls\.key}' | grep -q .
	"$(KUBECTL)" -n sentry-kubernetes-operator-system get secret sentry-hook-receiver-sources >/dev/null

deploy-registration: manifests kustomize verify-registration-prerequisites ## Deploy the opt-in registration writer after the controller install.
	"$(KUBECTL)" get namespace sentry-kubernetes-operator-system >/dev/null
	"$(KUSTOMIZE)" build config/state | "$(KUBECTL)" apply -f -
	KUSTOMIZE_BIN="$(KUSTOMIZE)" sh hack/render-kustomize-image.sh registration "$(IMG)" | "$(KUBECTL)" apply -f -

deploy-registration-production: verify-signed-image ## Deploy registration components only from a verified signed immutable image digest.
	$(MAKE) deploy-registration IMG="$(IMG)"

verify-registration-gateway-prerequisites: ## Verify Gateway API v1, public Services and the selected TLS/Passthrough listener.
	GATEWAY_NAME="$(GATEWAY_NAME)" GATEWAY_NAMESPACE="$(GATEWAY_NAMESPACE)" GATEWAY_LISTENER="$(GATEWAY_LISTENER)" TELEGRAM_HOSTNAME="$(TELEGRAM_HOSTNAME)" SENTRY_HOSTNAME="$(SENTRY_HOSTNAME)" KUBECTL="$(KUBECTL)" sh hack/manage-registration-gateway.sh verify

deploy-registration-gateway: verify-registration-gateway-prerequisites ## Attach both public receivers to an existing Gateway API TLS/Passthrough listener.
	GATEWAY_NAME="$(GATEWAY_NAME)" GATEWAY_NAMESPACE="$(GATEWAY_NAMESPACE)" GATEWAY_LISTENER="$(GATEWAY_LISTENER)" TELEGRAM_HOSTNAME="$(TELEGRAM_HOSTNAME)" SENTRY_HOSTNAME="$(SENTRY_HOSTNAME)" GATEWAY_WAIT_TIMEOUT="$(GATEWAY_WAIT_TIMEOUT)" KUBECTL="$(KUBECTL)" sh hack/manage-registration-gateway.sh deploy

verify-registration-monitoring-prerequisites: ## Verify Prometheus Operator monitoring CRDs before installing optional rules.
	"$(KUBECTL)" get crd servicemonitors.monitoring.coreos.com >/dev/null
	"$(KUBECTL)" get crd prometheusrules.monitoring.coreos.com >/dev/null

deploy-registration-monitoring: kustomize verify-registration-monitoring-prerequisites ## Deploy optional Telegram delivery ServiceMonitor and alert rules.
	"$(KUSTOMIZE)" build config/registration-monitoring | "$(KUBECTL)" apply -f -

verify-controller-monitoring-prerequisites: ## Verify Prometheus Operator monitoring CRDs before installing optional controller rules.
	"$(KUBECTL)" get crd servicemonitors.monitoring.coreos.com >/dev/null
	"$(KUBECTL)" get crd prometheusrules.monitoring.coreos.com >/dev/null

deploy-controller-monitoring: kustomize verify-controller-monitoring-prerequisites ## Deploy optional controller ServiceMonitor and alert rules.
	"$(KUSTOMIZE)" build config/controller-monitoring | "$(KUBECTL)" apply -f -

verify-controller-monitoring-access-prerequisites: ## Verify the exact Prometheus ServiceAccount and controller metrics-reader role.
	PROMETHEUS_SERVICE_ACCOUNT_NAME="$(PROMETHEUS_SERVICE_ACCOUNT_NAME)" PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE="$(PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE)" KUBECTL="$(KUBECTL)" sh hack/manage-controller-monitoring-access.sh verify

deploy-controller-monitoring-access: verify-controller-monitoring-access-prerequisites ## Authorize one exact Prometheus ServiceAccount to scrape authenticated controller metrics.
	PROMETHEUS_SERVICE_ACCOUNT_NAME="$(PROMETHEUS_SERVICE_ACCOUNT_NAME)" PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE="$(PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE)" KUBECTL="$(KUBECTL)" sh hack/manage-controller-monitoring-access.sh deploy

.PHONY: undeploy undeploy-registration undeploy-registration-gateway undeploy-registration-monitoring undeploy-controller-monitoring undeploy-controller-monitoring-access
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/state/rbac.yaml
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/state/routing-rbac.yaml
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/state/audit-rbac.yaml
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/state/audit-admission.yaml
	"$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f config/state/hook-source-rbac.yaml

undeploy-registration: kustomize ## Remove registration workloads/RBAC while preserving protected route state.
	"$(KUSTOMIZE)" build config/registration | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

undeploy-registration-gateway: ## Remove only the two rendered public TLSRoutes.
	GATEWAY_NAME="$(GATEWAY_NAME)" GATEWAY_NAMESPACE="$(GATEWAY_NAMESPACE)" GATEWAY_LISTENER="$(GATEWAY_LISTENER)" TELEGRAM_HOSTNAME="$(TELEGRAM_HOSTNAME)" SENTRY_HOSTNAME="$(SENTRY_HOSTNAME)" KUBECTL="$(KUBECTL)" ignore_not_found="$(ignore-not-found)" sh hack/manage-registration-gateway.sh delete

undeploy-registration-monitoring: kustomize ## Remove optional Telegram delivery monitoring resources.
	"$(KUSTOMIZE)" build config/registration-monitoring | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

undeploy-controller-monitoring: kustomize ## Remove optional controller monitoring resources.
	"$(KUSTOMIZE)" build config/controller-monitoring | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

undeploy-controller-monitoring-access: ## Remove only the exact Prometheus metrics-reader binding.
	PROMETHEUS_SERVICE_ACCOUNT_NAME="$(PROMETHEUS_SERVICE_ACCOUNT_NAME)" PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE="$(PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE)" KUBECTL="$(KUBECTL)" ignore_not_found="$(ignore-not-found)" sh hack/manage-controller-monitoring-access.sh delete

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= $(TOOLS_DIR)/bin/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(TOOLS_DIR)/bin/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0

#ENVTEST_VERSION is the controller-runtime release branch used for setup-envtest.
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v([0-9]+\.[0-9]+).*/release-\1/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: ## Verify the pinned controller-gen installed by install-t00-tools.
	@test -x "$(CONTROLLER_GEN)" || { echo "run make install-t00-tools first" >&2; exit 1; }

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" "$(GO)" install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

##@ T00 compatibility evidence

.PHONY: install-t00-tools verify-t00-tools verify-envtest verify-t00 verify-webhook-contract verify-name-resolution build-t00-kind-image verify-kind prepare-kind-probes verify-kubernetes-probes verify-kind-admission verify-leader-election verify-t01-generated verify-t01-crd verify-t01-webhook-kind verify-t02 verify-t03 verify-t04 verify-t04-kind verify-t05 verify-t06-core verify-t06-kind-rbac verify-t07-core verify-t08-core verify-t08-kind-ca-rotation verify-t08-kind-queue-soak verify-t08-kind-gateway-api verify-t08-kind-gateway-dataplane verify-t08-kind-monitoring verify-t09-core verify-t09-kind-multinode-failover verify-t09-kind-monitoring-access verify-t09-kind-controller-monitoring verify-t10-failure-injection

install-t00-tools:
	sh hack/install-t00-tools.sh

verify-t00-tools:
	sh hack/verify-t00-tools.sh

verify-envtest:
	sh hack/verify-envtest.sh

verify-t00:
	sh hack/verify-t00.sh

verify-webhook-contract:
	sh hack/verify-webhook-contract.sh

verify-name-resolution:
	sh hack/verify-name-resolution.sh

build-t00-kind-image:
	sh hack/build-t00-kind-image.sh $(KUBERNETES_VERSION)

verify-kind:
	sh hack/verify-kind.sh $(KIND_CONTEXT)

prepare-kind-probes:
	sh hack/prepare-kind-probes.sh $(KIND_CONTEXT)

verify-kubernetes-probes: prepare-kind-probes
	sh hack/verify-kubernetes-probes.sh $(KIND_CONTEXT)

verify-kind-admission:
	sh hack/verify-kind-admission.sh $(KIND_CONTEXT)

verify-leader-election:
	sh hack/verify-leader-election.sh $(KIND_CONTEXT)

verify-t01-generated: manifests generate
	sh hack/verify-t01-generated.sh

verify-t01-crd: verify-t01-generated
	sh hack/verify-t01-crd.sh $(KIND_CONTEXT)

verify-t01-webhook-kind: manifests generate
	sh hack/verify-t01-webhook-kind.sh $(KIND_CONTEXT)

verify-t02:
	sh hack/verify-t02.sh

verify-t03: manifests generate
	sh hack/verify-t03.sh

verify-t04: manifests generate
	sh hack/verify-t04.sh

verify-t04-kind:
	sh hack/verify-t04-kind.sh $(KIND_CONTEXT)

verify-t05: manifests generate
	sh hack/verify-t05.sh

verify-t06-core: manifests generate
	sh hack/verify-t06-core.sh

verify-t06-kind-rbac:
	sh hack/verify-t06-kind-rbac.sh $(KIND_CONTEXT)

verify-t07-core:
	sh hack/verify-t07-core.sh

verify-t08-core: manifests generate
	sh hack/verify-t08-core.sh

verify-t08-kind-ca-rotation:
	sh hack/verify-t08-kind-ca-rotation.sh $(KIND_CONTEXT)

verify-t08-kind-queue-soak:
	sh hack/verify-t08-kind-queue-soak.sh $(KIND_CONTEXT)

verify-t08-kind-gateway-api:
	sh hack/verify-t08-kind-gateway-api.sh

verify-t08-kind-gateway-dataplane:
	sh hack/verify-t08-kind-gateway-dataplane.sh

verify-t08-kind-monitoring:
	sh hack/verify-t08-kind-monitoring.sh

verify-t09-core: manifests generate
	sh hack/verify-t09-core.sh

verify-t09-kind-multinode-failover:
	sh hack/verify-t09-kind-multinode-failover.sh

verify-t09-kind-monitoring-access:
	sh hack/verify-t09-kind-monitoring-access.sh $(KIND_CONTEXT)

verify-t09-kind-controller-monitoring:
	sh hack/verify-t09-kind-controller-monitoring.sh

verify-t10-failure-injection:
	sh hack/verify-t10-failure-injection.sh

define gomodver
$(shell "$(GO)" list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
