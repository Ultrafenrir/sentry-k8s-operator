# Configuration

`SentryConnection` is cluster-scoped. Its referenced token and optional CA bundle Secrets are
read only from `sentry-kubernetes-operator-system`. Access rules authorize namespaced projects
by owner, namespace, Sentry team, create permission, existing project slugs, and notification
integration catalogs.

`SentryProject` is namespaced. It can create one managed Sentry project or observe one existing
project. Managed settings, client keys, DSN output, team access, destinations, and alerts are
declared in the CR. External identities and credentials are not exposed in status.

`SentryAlertPolicy` is cluster-scoped and reusable. It does not change Sentry by itself; a
`SentryProject` opts in through `spec.alertPolicy`.

The CRDs use structural OpenAPI validation but do not require Kubernetes CEL validation support.
Cross-field configuration errors are surfaced through resource status during reconciliation.

The supported notification providers are Slack and PagerDuty. They must already be installed in
Sentry and declared by readable aliases in the connection. The operator does not install provider
integrations.

Plain HTTP for the Sentry API is disabled by default. For a trusted development environment only,
set `spec.transport.allowInsecureHTTP: true`. Controller Prometheus metrics are independently
served over HTTP.
