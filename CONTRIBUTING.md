# Contributing

Keep changes focused on the Sentry connection, project, or alert-policy controller. Repository
content must be English. Do not commit credentials or protected state.

```sh
make generate manifests
make test
make helm-lint
make verify-english
git diff --check
```

Generated CRDs and RBAC must be updated from their Go markers.
