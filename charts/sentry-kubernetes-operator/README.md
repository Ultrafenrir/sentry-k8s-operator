# Helm chart

The chart installs the operator CRDs, controller, HTTP metrics Service, protected state RBAC, and
one cluster-scoped `SentryConnection`. It does not create any `SentryProject` or
`SentryAlertPolicy` instances.

Required values are under `image` and `sentryConnection`. The connection token Secret must exist
in `sentry-kubernetes-operator-system`; do not put the token in values. See the repository
[README](../../README.md) for a complete installation command and values example.

`controllerMonitoring.enabled=true` additionally renders a Prometheus Operator `ServiceMonitor`.
The scrape endpoint is plain HTTP.
