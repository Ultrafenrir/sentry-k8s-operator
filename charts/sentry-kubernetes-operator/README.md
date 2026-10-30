# Sentry Kubernetes Operator chart

This alpha chart is generated from the repository's canonical Kustomize manifests.
cert-manager is required. Telegram registration is disabled by default and additionally
requires the administrator-provided Secrets documented in the repository README.

`registrationGateway.enabled=true` optionally renders two Gateway API v1 `TLSRoute`
resources for the Telegram and Sentry public receivers. It requires registration to be
enabled, distinct exact public hostnames, and an existing `TLS`/`Passthrough` listener that
allows routes from `sentry-kubernetes-operator-system`. The Gateway implementation, public
address, DNS, and certificates remain installation-owned.

`controllerMonitoring.enabled=true` renders the authenticated TLS ServiceMonitor and rules.
Because the metrics endpoint authorizes the scraper's bearer token, optionally set
`controllerMonitoring.access.enabled=true` with the exact Prometheus ServiceAccount name
and namespace. This renders one fixed-subject binding to the chart's metrics-reader role;
the default chart grants no scraper identity.

Production rendering must set `production=true` and `image.digest=sha256:...`. The three
protected namespaces carry `helm.sh/resource-policy: keep`; Helm uninstall removes their
RBAC but preserves the namespaces and their binding/routing/queue Secrets. Review those
namespaces before any deliberate manual deletion.

Do not edit `files/` or `crds/` manually. Run `make helm-generate` and
`make verify-helm` after changing Kustomize manifests.
