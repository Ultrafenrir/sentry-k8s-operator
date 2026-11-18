# Helm chart

The chart installs the operator CRDs, one controller, an HTTP metrics Service, RBAC, and one
cluster-scoped `SentryConnection`. It does not create any `SentryProject` or
`SentryAlertPolicy`, `SentryCronMonitor`, or `SentryUptimeMonitor` instances. Every namespaced object uses the Helm release namespace, and every
workload name is derived from the Helm release name. The chart does not create namespaces.
When `sentryConnection.name` is empty, the connection name is also the Helm release name.

Required values are under `image` and `sentryConnection`. The connection token Secret must exist
in the Helm release namespace; do not put the token in values. See the repository
[README](../../README.md) for a complete installation command and values example.

Application namespaces pre-create `sentry-dsns`. The controller patches the key named after each
`SentryProject`; chart RBAC does not permit creating or deleting application Secrets.

`controllerMonitoring.enabled=true` additionally renders a Prometheus Operator `ServiceMonitor`.
The scrape endpoint is plain HTTP.
