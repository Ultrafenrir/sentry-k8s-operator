# Development runbook

Prerequisites are Go 1.26, Make, Docker, kubectl, Helm, `controller-gen`, and the custom
`golangci-lint` binary built from `.custom-gcl.yml` with `golangci-lint custom`.
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

## Local Sentry

The Makefile manages a pinned, version-scoped self-hosted Sentry checkout. `sentry-up` recreates
only that version's local runtime and volumes, then starts the API-facing core services and their
direct databases. Event consumers, task workers, schedulers, cleanup jobs, and optional product
services are not started.

Run the same workflow for either pinned compatibility target:

```sh
for version in 24.7.1 26.10.1; do
  make verify-sentry-compose SENTRY_VERSION="$version"
  make sentry-up SENTRY_VERSION="$version" SENTRY_SKIP_PULL=true
  make sentry-bootstrap SENTRY_VERSION="$version"
  make sentry-smoke SENTRY_VERSION="$version"
  make sentry-status SENTRY_VERSION="$version"
  make sentry-down SENTRY_VERSION="$version"
done
```

The selected core-service list is version-specific. Sentry 24.7.1 does not define the
`pgbouncer` or `seaweedfs` Compose services that are part of the 26.10.1 core profile.

Bootstrap creates a local organization, team, API user, and non-delivery catalog fixtures. Its
API token is stored under the ignored `.cache/sentry-self-hosted/tokens` directory with mode
`0600`; commands must never print or commit it. Slack acceptance still requires an installed
Slack integration or a test double for Slack's external API. The operator verifies Sentry
configuration, not third-party message delivery.

`sentry-down` removes only the selected stand's containers, network, token, and version-scoped
volumes.
