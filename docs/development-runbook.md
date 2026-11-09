# Development runbook

Prerequisites are Go 1.26, Make, Docker, kubectl, Helm, and `controller-gen`.

```sh
git status --short
make help
make generate manifests
make test
make helm-lint
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
