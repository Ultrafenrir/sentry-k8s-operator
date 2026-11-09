# Project status

The repository is an alpha Sentry operator centered on `SentryConnection`, `SentryProject`, and
`SentryAlertPolicy`. The controller supports project lifecycle, DSN publication, selected project
settings, Slack and PagerDuty destination resolution, and version-pinned legacy or modern Sentry
alert adapters.

The default chart installs the operator and one connection. Project and policy instances belong
to application teams. Metrics use HTTP. The deployment has no admission server, certificate
manager, or network-policy dependency.

Compatibility with a Kubernetes or Sentry version must be demonstrated by tests against that
version; the Helm chart does not block installation based on the Kubernetes server version.

Live event ingestion and third-party notification delivery are outside the evidence provided by
unit tests and rendered manifests.

CI runs `make lint` and the complete `make test` suite. The test target includes unit tests for
project authorization, protected state, destination resolution, alert adapters, and audit state,
plus controller envtest coverage against a local Kubernetes API server.
