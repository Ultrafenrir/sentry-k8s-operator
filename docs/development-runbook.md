# Development runbook

Prerequisites are Go 1.26, Make, Docker, kubectl, Helm, and `controller-gen`.
Controller tests also need envtest binaries. `make test` uses `KUBEBUILDER_ASSETS`, a local
`.tools/envtest/1.35.0` installation, or `setup-envtest`, in that order.
The Makefile keeps the Go build cache separate for every patch version so switching Go toolchains
cannot reuse incompatible standard-library archives.

```sh
git status --short
make help
make generate manifests
make test
make lint
make verify-english
git diff --check
```

Generated CRDs come from API markers. Generated RBAC comes from controller markers. `make
manifests` also copies the generated CRDs into the Helm chart. Render canonical deployment files
with:

```sh
kubectl kustomize config/chart -o charts/sentry-kubernetes-operator/files/operator.yaml
kubectl kustomize config/state -o charts/sentry-kubernetes-operator/files/state.yaml
```

Build the single controller image with `make docker-build IMG=repository/name:tag`.
