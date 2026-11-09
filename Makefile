IMG ?= ultrafenrir/sentry-k8s-operator:dev
GO ?= $(if $(wildcard .tools/go-1.26.10/bin/go),$(CURDIR)/.tools/go-1.26.10/bin/go,go)
CONTROLLER_GEN ?= $(if $(wildcard .tools/bin/controller-gen),$(CURDIR)/.tools/bin/controller-gen,controller-gen)
HELM ?= helm
ENVTEST_K8S_VERSION ?= 1.35.0
ENVTEST_ASSETS ?= $(CURDIR)/.tools/envtest/$(ENVTEST_K8S_VERSION)
GO_VERSION := $(shell env -u GOROOT $(GO) env GOVERSION)
GOROOT := $(shell env -u GOROOT $(GO) env GOROOT)
GOTOOLCHAIN := local
GOCACHE := $(CURDIR)/.cache/go-build/$(GO_VERSION)
export GOROOT
export GOTOOLCHAIN
export GOCACHE

.PHONY: help generate manifests fmt vet lint test build docker-build docker-push helm-lint verify-english clean

help: ## Show the available targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*?##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

generate: ## Generate DeepCopy implementations.
	PATH="$(dir $(GO)):$$PATH" $(CONTROLLER_GEN) object paths=./api/...

manifests: ## Generate Helm CRDs.
	PATH="$(dir $(GO)):$$PATH" $(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=charts/sentry-kubernetes-operator/crds

fmt: ## Format Go source.
	$(GO) fmt ./...

vet: ## Run go vet.
	$(GO) vet ./...

lint: generate manifests fmt vet helm-lint verify-english ## Run all static and generated checks.
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

verify-english: ## Reject Cyrillic text in repository files.
	@if git grep -n -I -P '[\x{0400}-\x{04FF}]' -- .; then echo 'Repository text must be English.'; exit 1; fi

clean: ## Remove local build output.
	rm -f bin/manager
