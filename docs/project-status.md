# Project status

The repository is an alpha Sentry operator with five public CRDs: `SentryConnection`,
`SentryProject`, `SentryAlertPolicy`, `SentryCronMonitor`, and `SentryUptimeMonitor`. The controller supports project lifecycle, DSN publication, selected project
settings, Slack and PagerDuty destination resolution, and version-pinned legacy or modern Sentry
alert adapters.

PagerDuty resolution is deliberately installation-neutral. Administrators install the integration
and add services in Sentry; the connection catalog only names those existing objects. A protected
service identity prevents silent replacement, but the service must remain visible through Sentry's
public integration response. An opaque service catalog fails closed.

The modern alert adapter also supports a bounded Application Metrics selector and one binary
equation without exposing raw Sentry query syntax. Local acceptance ingests counter, gauge, and
distribution fixtures through the official SDK and verifies their catalog visibility. Dynamic
detection probes Seer before mutation and reports a stable unavailable reason on the local stand.
Modern issue workflows also support a typed, catalog-bound GitHub or GitHub Enterprise issue
action. The connection owns readable integration and repository aliases; the project and policy
never carry numeric integration IDs.

The bounded Application Metrics model is the final query surface for this release: raw Sentry
queries and arbitrary integration payloads remain intentionally unsupported. Seer workflow actions
remain outside the public API because their create, read-after-write, and deletion contracts are not
stable enough for controller ownership. Source-fetching credentials and organization-wide privacy
baselines are likewise explicit non-goals; adding either requires a separate product decision and a
new compatibility and secret-handling design.

The default chart installs the operator and one connection. Project, policy, and monitor instances
belong to application teams. Metrics use HTTP. The deployment has no admission server, certificate
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
evidence. The modern acceptance covers every managed project component and all five CRDs, including
create, update, disable, delete, Retain/no-adoption behavior, browser inspection, and repeated
operator restarts. The exact release checklist is `docs/acceptance-runbook.md`.

Sentry's automatically created Error Monitor is outside operator ownership. It is observed in
physical acceptance but is not disabled, modified, or deleted by `SentryProject` or
`SentryAlertPolicy`.

The complete human-readable field guide is `docs/configuration.md`. It documents every public block
of all five CRDs, typed alert and monitor examples, omitted-versus-empty ownership semantics, version
compatibility, and status troubleshooting. `docs/quickstart.md` uses the same typed alert API.

CI runs `make lint` and the complete `make test` suite. The test target includes unit tests for
project validation, protected state, destination resolution, alert adapters, and audit state,
plus controller envtest coverage against a local Kubernetes API server.

The automated upgrade lane preserves a previous-release project and protected binding through the
documented breaking alpha object migration, chart/CRD upgrade, a current-controller restart, and
guarded deletion. Binary rollback across the schema break is intentionally unsupported.
