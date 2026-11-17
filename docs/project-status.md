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
Generated CRDs intentionally avoid CEL `x-kubernetes-validations` so older API servers can install
them. The chart renders CRD v1 resources as far back as Kubernetes 1.16, omits pod-level seccomp
fields before 1.19, and selects the appropriate PodDisruptionBudget API around the 1.21 boundary.
Controller runtime behavior is tested by envtest against Kubernetes 1.35 and 1.36; older runtime
compatibility remains best-effort until an envtest or cluster lane demonstrates it. Cross-field
errors are reported by controller reconciliation before any Sentry API call instead of by CEL
admission.

Project, destination, and alert compatibility claims require repeatable end-to-end evidence
against the declared Sentry versions. The operator does not ingest events or deliver third-party
notifications; those are Sentry responsibilities, not additional project components.

The local acceptance workflow covers both pinned alert API families. It verifies the remote
project and alert objects through Sentry's API and UI, controller restart reconciliation without
CR changes, credential and Sentry outages, shared DSN Secret preservation, and `Retain` versus
`DeleteProject` lifecycle behavior. Legacy acceptance includes issue, static metric, change metric,
and project-local metric rules; a ready Kubernetes condition alone is not considered sufficient
evidence. The modern acceptance covers every managed project component and all three CRDs, including
create, update, delete, and repeated operator restarts.

The complete human-readable field guide is `docs/configuration.md`. It documents every public block
of all three CRDs, typed alert examples, omitted-versus-empty ownership semantics, version
compatibility, and status troubleshooting. `docs/quickstart.md` uses the same typed alert API.

CI runs `make lint` and the complete `make test` suite. The test target includes unit tests for
project validation, protected state, destination resolution, alert adapters, and audit state,
plus controller envtest coverage against a local Kubernetes API server.
