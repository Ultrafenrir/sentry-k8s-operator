IMG ?= ultrafenrir/sentry-k8s-operator:dev
GO ?= go
CONTROLLER_GEN ?= controller-gen
HELM ?= helm

.PHONY: help generate manifests fmt vet test build docker-build docker-push helm-lint verify-english clean

help: ## Show the available targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\n"} /^[a-zA-Z_0-9-]+:.*?##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

generate: ## Generate DeepCopy implementations.
	$(CONTROLLER_GEN) object paths=./api/...

manifests: ## Generate CRDs and controller RBAC.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/*.yaml charts/sentry-kubernetes-operator/crds/

fmt: ## Format Go source.
	$(GO) fmt ./...

vet: ## Run go vet.
	$(GO) vet ./...

test: generate manifests fmt vet ## Run the Go test suite.
	$(GO) test ./...

build: generate fmt vet ## Build the operator binary.
	$(GO) build -o bin/manager ./cmd

docker-build: ## Build the operator image for the current Docker platform.
	docker build -t $(IMG) .

docker-push: ## Push the operator image.
	docker push $(IMG)

helm-lint: ## Lint and render the Helm chart.
	$(HELM) lint charts/sentry-kubernetes-operator
	$(HELM) template sentry-operator charts/sentry-kubernetes-operator >/dev/null

verify-english: ## Reject Cyrillic text in repository files.
	@if rg -n -P '[\x{0400}-\x{04FF}]' --glob '!.git/**' .; then echo 'Repository text must be English.'; exit 1; fi

clean: ## Remove local build output.
	rm -f bin/manager
