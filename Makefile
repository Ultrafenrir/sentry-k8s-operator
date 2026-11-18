IMG ?= ultrafenrir/sentry-k8s-operator:dev
GO ?= $(if $(wildcard .tools/go-1.26.10/bin/go),$(CURDIR)/.tools/go-1.26.10/bin/go,go)
CONTROLLER_GEN ?= $(if $(wildcard .tools/bin/controller-gen),$(CURDIR)/.tools/bin/controller-gen,controller-gen)
GOLANGCI_LINT ?= $(if $(wildcard .tools/bin/golangci-lint),$(CURDIR)/.tools/bin/golangci-lint,golangci-lint)
HELM ?= helm
ENVTEST_K8S_VERSION ?= 1.35.0
ENVTEST_ASSETS ?= $(CURDIR)/.tools/envtest/$(ENVTEST_K8S_VERSION)
SENTRY_VERSION ?= 26.10.1
SENTRY_PROFILE ?= errors-only
SENTRY_PORT ?=
SENTRY_WAIT_TIMEOUT ?= 1800
SENTRY_SKIP_PULL ?= false
SENTRY_SELF_HOSTED_CACHE ?= $(CURDIR)/.cache/sentry-self-hosted
SENTRY_BASH_BIN ?=
GO_VERSION := $(shell env -u GOROOT $(GO) env GOVERSION)
GOROOT := $(shell env -u GOROOT $(GO) env GOROOT)
GOTOOLCHAIN := local
GOCACHE := $(CURDIR)/.cache/go-build/$(GO_VERSION)
GOLANGCI_LINT_CACHE := $(CURDIR)/.cache/golangci-lint
export GOROOT
export GOTOOLCHAIN
export GOCACHE
export GOLANGCI_LINT_CACHE

.PHONY: help generate manifests fmt vet lint lint-go test build docker-build docker-push helm-lint verify-english verify-upgrade sentry-metrics-fixture clean verify-sentry-compose sentry-up sentry-bootstrap sentry-smoke sentry-status sentry-logs sentry-down

help: ## Show the available targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*?##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

verify-sentry-compose: ## Verify the pinned, isolated local Sentry Compose stack.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh verify

sentry-up: ## Recreate the selected local Sentry profile (errors-only or feature-complete).
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SKIP_PULL="$(SENTRY_SKIP_PULL)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" SENTRY_BASH_BIN="$(SENTRY_BASH_BIN)" sh hack/manage-sentry-self-hosted.sh up

sentry-bootstrap: ## Create a local API user and store its token outside Git.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh bootstrap

sentry-smoke: ## Check the selected local Sentry health endpoint.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh smoke

sentry-status: ## Show only the selected local Sentry core containers and volumes.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh status

sentry-logs: ## Show recent logs for the selected local Sentry core stack.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh logs

sentry-down: ## Remove the selected local Sentry runtime and managed volumes.
	SENTRY_VERSION="$(SENTRY_VERSION)" SENTRY_PROFILE="$(SENTRY_PROFILE)" SENTRY_PORT="$(SENTRY_PORT)" SENTRY_WAIT_TIMEOUT="$(SENTRY_WAIT_TIMEOUT)" SENTRY_SELF_HOSTED_CACHE="$(SENTRY_SELF_HOSTED_CACHE)" sh hack/manage-sentry-self-hosted.sh down

generate: ## Generate DeepCopy implementations.
	PATH="$(dir $(GO)):$$PATH" $(CONTROLLER_GEN) object paths=./api/...

manifests: ## Generate Helm CRDs.
	PATH="$(dir $(GO)):$$PATH" $(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=charts/sentry-kubernetes-operator/crds

fmt: ## Format Go source.
	$(GO) fmt ./...

vet: ## Run go vet.
	$(GO) vet ./...

lint-go: ## Run golangci-lint with the repository configuration.
	PATH="$(dir $(GO)):$$PATH" $(GOLANGCI_LINT) run ./...

lint: generate manifests fmt vet lint-go helm-lint verify-english ## Run all static and generated checks.
	git diff --check

test: generate manifests fmt vet ## Run the complete Go test suite, including envtest.
	@if [ -n "$$KUBEBUILDER_ASSETS" ]; then \
		$(GO) test ./...; \
	elif [ -x "$(ENVTEST_ASSETS)/etcd" ] && [ -x "$(ENVTEST_ASSETS)/kube-apiserver" ]; then \
		KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" $(GO) test ./...; \
	elif command -v setup-envtest >/dev/null 2>&1; then \
		assets="$$(setup-envtest use -p path $(ENVTEST_K8S_VERSION))"; \
		KUBEBUILDER_ASSETS="$$assets" $(GO) test ./...; \
	else \
		echo "envtest binaries are missing; install setup-envtest or set KUBEBUILDER_ASSETS"; \
		exit 1; \
	fi

verify-upgrade: ## Verify the documented alpha API migration, restart, and deletion against local Sentry 26.
	sh hack/verify-upgrade.sh

sentry-metrics-fixture: ## Ingest and verify three deterministic Application Metrics in local Sentry 26.
	sh hack/ingest-application-metrics.sh

build: generate fmt vet ## Build the operator binary.
	$(GO) build -o bin/manager ./cmd

docker-build: ## Build the operator image for the current Docker platform.
	docker build -t $(IMG) .

docker-push: ## Push the operator image.
	docker push $(IMG)

helm-lint: ## Lint and render the Helm chart.
	$(HELM) lint charts/sentry-kubernetes-operator
	mkdir -p .cache/helm
	$(HELM) template helm-contract charts/sentry-kubernetes-operator --namespace helm-contract-ns --include-crds \
		--set sentryConnection.spec.credentials.tokenSecretRef.name=helm-contract-token \
		--set sentryConnection.spec.credentials.tokenSecretRef.key=api-token >.cache/helm/rendered.yaml
	@grep -q '^  name: helm-contract-controller$$' .cache/helm/rendered.yaml
	@grep -q '^  namespace: helm-contract-ns$$' .cache/helm/rendered.yaml
	@grep -q '^      name: helm-contract-token$$' .cache/helm/rendered.yaml
	@grep -q '^      key: api-token$$' .cache/helm/rendered.yaml
	@awk 'BEGIN { core = 0; found = 0 } /apiGroups: \[""\]/ { core = 1; next } core && /resources: \["events"\]/ { found = 1 } /^  - apiGroups:/ { core = 0 } END { exit found ? 0 : 1 }' .cache/helm/rendered.yaml
	@if grep -Eq '^kind: Namespace$$|sentry-kubernetes-operator-system|sentry-kubernetes-operator-state|sentry-kubernetes-operator-audit' .cache/helm/rendered.yaml; then echo 'Helm render contains fixed namespaces'; exit 1; fi
	$(HELM) template helm-contract charts/sentry-kubernetes-operator --namespace helm-contract-ns --kube-version 1.16.0 \
		--set sentryConnection.spec.credentials.tokenSecretRef.name=helm-contract-token \
		--set sentryConnection.spec.credentials.tokenSecretRef.key=api-token >.cache/helm/rendered-1.16.yaml
	@if grep -q 'seccompProfile:' .cache/helm/rendered-1.16.yaml; then echo 'Kubernetes 1.16 render contains unsupported seccompProfile'; exit 1; fi
	$(HELM) template helm-contract charts/sentry-kubernetes-operator --namespace helm-contract-ns --kube-version 1.20.0 \
		--set replicaCount=2 \
		--set sentryConnection.spec.credentials.tokenSecretRef.name=helm-contract-token \
		--set sentryConnection.spec.credentials.tokenSecretRef.key=api-token >.cache/helm/rendered-1.20.yaml
	@grep -q '^apiVersion: policy/v1beta1$$' .cache/helm/rendered-1.20.yaml
	$(HELM) template helm-contract charts/sentry-kubernetes-operator --namespace helm-contract-ns --kube-version 1.21.0 \
		--set replicaCount=2 \
		--set sentryConnection.spec.credentials.tokenSecretRef.name=helm-contract-token \
		--set sentryConnection.spec.credentials.tokenSecretRef.key=api-token >.cache/helm/rendered-1.21.yaml
	@grep -q '^apiVersion: policy/v1$$' .cache/helm/rendered-1.21.yaml
	@grep -q 'seccompProfile:' .cache/helm/rendered-1.21.yaml

verify-english: ## Reject Cyrillic text in repository files.
	@if git grep -n -I -P '[\x{0400}-\x{04FF}]' -- .; then echo 'Repository text must be English.'; exit 1; fi

clean: ## Remove local build output.
	rm -rf bin .cache/go-build .cache/golangci-lint .cache/helm
