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

Generated CRDs come from API markers and are written directly into the Helm chart. All runtime
resources are ordinary Helm templates. Render them with:

```sh
helm template my-sentry-operator charts/sentry-kubernetes-operator \
  --namespace observability \
  --include-crds
```

Build the single controller image with `make docker-build IMG=repository/name:tag`.
