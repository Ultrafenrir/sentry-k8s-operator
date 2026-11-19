# Configuration reference

This guide documents the complete public configuration surface of the five CRDs. The generated
CRDs remain the machine-readable source of validation constraints; this document explains how to
choose values and what the operator owns.

## Common rules

- `SentryConnection` and `SentryAlertPolicy` are cluster-scoped. `SentryProject`,
  `SentryCronMonitor`, and `SentryUptimeMonitor` are namespaced.
- The default Helm chart installs the operator and one connection. Application teams own project
  and policy instances.
- An omitted optional field is unmanaged. For owned maps and lists, an explicitly empty value
  removes only children previously created and protected by this operator.
- Cross-field and Sentry-version checks run in the controller before external mutation. The CRDs
  intentionally avoid CEL validation so they can be installed on older Kubernetes API servers.
- Public DSNs are written only to the pre-created `sentry-dsns` Secret in the project namespace.
  The key is the `SentryProject` name. Optional symbol-source credentials are read only from the
  pre-created `sentry-project-inputs` Secret. Uptime request values can only be read from the
  pre-created `sentry-uptime-inputs` Secret in the monitor namespace.
- API tokens, secret keys, private DSNs, and protected Sentry identities never belong in a CR or
  status field.

## `SentryConnection`

A connection selects one Sentry organization and defines platform-owned credentials, transport,
compatibility, regional fencing, and readable aliases for integrations that already exist in
Sentry. It never creates users, teams, repositories, or integrations.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: company-sentry
spec:
  url: https://sentry.example.com
  organization: company
  credentials:
    tokenSecretRef:
      name: sentry-api-token
      key: token
    caBundleSecretRef:
      name: sentry-private-ca
      key: ca.crt
  compatibility:
    alerts: modern-26.10.1
  ownership:
    region: eu-central
    role: Active
    revision: 1
  projectManagement:
    unmanagedProjects: Reject
  integrations:
    slack:
      workspaces:
        - name: company-slack
          sentryName: Company Workspace
    pagerDuty:
      accounts:
        - name: company-pagerduty
          sentryName: Company PagerDuty
    sourceCode:
      installations:
        - name: company-github
          provider: github
          sentryName: Company GitHub
          repositories:
            - name: checkout
              sentryName: company/checkout
```

### Connection fields

| Path | Meaning |
| --- | --- |
| `spec.url` | Sentry base URL. HTTPS is required unless the local-development exception is enabled. |
| `spec.organization` | Exact Sentry organization slug. |
| `spec.credentials.tokenSecretRef` | API token key in a Secret in the operator namespace. |
| `spec.credentials.caBundleSecretRef` | Optional PEM CA bundle key in the operator namespace. |
| `spec.transport.allowInsecureHTTP` | Allows plain HTTP for a trusted local stand only. |
| `spec.transport.insecureSkipTLSVerify` | Disables HTTPS certificate and hostname verification. It cannot be combined with a CA bundle. |
| `spec.compatibility.alerts` | Explicit alert API profile: `legacy-24.7.1` or `modern-26.10.1`. |
| `spec.ownership.region` | Stable regional installation name used for fencing. |
| `spec.ownership.role` | `Active` may mutate; `Standby` observes protected ownership and does not steal it. |
| `spec.ownership.revision` | Monotonically increasing platform fencing revision. |
| `spec.projectManagement.unmanagedProjects` | `Reject`, `Standby`, or guarded `Adopt` behavior for an existing project with no recognized ownership marker. |

Integration catalog entries use a stable lowercase local `name` and the exact display name visible
in Sentry as `sentryName`. Source-code providers are `github`, `github_enterprise`, `gitlab`,
`bitbucket`, or `vsts`. Declaring a catalog entry only makes it selectable by projects and policies.
It never installs Slack, PagerDuty, or source-code integrations and never creates PagerDuty
services or routing keys. PagerDuty resolution requires Sentry's public integration response to
expose the selected service table. If that table is absent, including after a restart, the target
fails closed instead of treating a previously pinned identity as proof that the service still
exists.

For a normal single-region installation, omit `ownership`. Use `Reject` unless there is a reviewed
migration requirement for existing Sentry projects.

## `SentryAlertPolicy`

A policy is a reusable alert template. Creating the policy does not call Sentry. A project opts in
with the backward-compatible `spec.alertPolicy` or ordered `spec.alertPolicies`, after which policy
alerts and project-local alerts are merged by stable map key. Policy order is durable, but does not
imply override precedence: any duplicate destination or alert key fails closed before Sentry I/O.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryAlertPolicy
metadata:
  name: backend-alerts
spec:
  sentry: company-sentry
  notificationTargets:
    pagerDuty:
      company-pagerduty:
        - Backend On-call
  alerts:
    new-production-issues:
      enabled: true
      description: New production issue
      ownerTeam: backend
      throttleMinutes: 10
      issue:
        triggerMatch: any
        triggers:
          - type: first-seen
          - type: regression
        filterMatch: all
        filters:
          - type: level
            operator: gte
            value: error
        environment: production
      actions:
        - status: triggered
          email:
            target: team
            team: backend
    error-volume:
      enabled: true
      description: Elevated production errors
      ownerTeam: backend
      throttleMinutes: 30
      metric:
        preset: error-count
        environment: production
        intervalMinutes: 5
        detection:
          static:
            direction: above
            high: 100
            medium: 50
            resolve: 10
      actions:
        - status: high
          destination: pagerDuty/Backend On-call
          pagerDutySeverity: critical
```

### Policy fields

| Path | Meaning |
| --- | --- |
| `spec.sentry` | Name of the connection this policy is valid for. |
| `spec.notificationTargets` | Readable Slack channels and PagerDuty services used by policy actions. |
| `spec.alerts` | Up to 100 alert rules keyed by stable local name. |

`notificationTargets.slack` maps a connection workspace alias to channel names.
`notificationTargets.pagerDuty` maps an account alias to service names. Actions refer to flattened
names such as `slack/backend-alerts` or `pagerDuty/Backend On-call`. A channel or service name must
be unique within its provider in one effective project.

### Common alert fields

| Field | Meaning |
| --- | --- |
| `enabled` | Enables or mutes the managed rule. Defaults to `true`. |
| `issue` / `metric` | Exactly one typed rule kind for new manifests. |
| `actions` | Ordered email or destination actions. |
| `ownerTeam` | Sentry team slug shown as the alert owner. |
| `description` | Human-readable purpose of the rule. |
| `throttleMinutes` | `0` means every trigger; otherwise a bounded notification interval. |

The older `trigger`, `windowMinutes`, `threshold`, and `notify` fields remain only for compatibility
with existing manifests. New configuration should use typed `issue`, `metric`, and `actions` blocks.

### Issue alerts

`issue.triggerMatch` and `issue.filterMatch` are `all` or `any`. Supported trigger types are:

| Type | Additional fields |
| --- | --- |
| `first-seen`, `regression`, `resolved`, `escalation` | None. |
| `event-count`, `user-count`, `session-percentage` | Positive `windowMinutes` and `threshold`. |

Filters have `type`, `operator`, optional `key`, and string `value`. Types are `age`, `assignment`,
`category`, `frequency`, `priority`, `type`, `event-attribute`, `tag`, `latest-release`,
`release-age`, and `level`. Operators are `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, and `contains`.
The controller rejects combinations unsupported by the selected compatibility profile.

`issue.environment` is optional. On Sentry 24, the environment must already exist in Sentry before
an alert can reference it; environments are normally created by ingested events.

### Metric alerts

Metric presets are `error-count`, `affected-users`, `crash-free-sessions`, `crash-free-users`,
`throughput`, `duration`, `failure-rate`, `largest-contentful-paint`, and modern-only `logs` and
`application-metric`.
`intervalMinutes` is the evaluation window and `environment` is optional.

Exactly one detection block is required:

```yaml
detection:
  static:
    direction: above
    high: 100
    medium: 50       # optional
    resolve: 10      # optional
```

```yaml
detection:
  change:
    direction: above
    highPercent: 50
    mediumPercent: 25    # optional
    compareMinutes: 60
    resolvePercent: 5    # optional
```

```yaml
detection:
  dynamic:
    responsiveness: medium
    direction: both
```

Directions are `above` or `below`. Dynamic responsiveness is `high`, `medium`, or `low`; dynamic
direction is `above`, `below`, or `both`. Dynamic detection requires the modern profile and a Sentry
deployment with the required Seer support. It is rejected when that backend capability is absent.
The failure is reported as `DynamicDetectionUnavailable` before a workflow or detector is created.

The `application-metric` preset selects one exact catalog metric without accepting Sentry query
syntax:

```yaml
metric:
  preset: application-metric
  applicationMetric:
    left:
      name: checkout.latency
      type: distribution
      unit: millisecond
      aggregation: p95
  intervalMinutes: 5
  detection:
    static:
      direction: above
      high: 500
      resolve: 100
```

Types are `counter`, `gauge`, and `distribution`. Counter metrics support `sum`, `per-second`, and
`per-minute`; gauges support `count`, `sum`, `avg`, `min`, `max`, and the two rate aggregations;
distributions additionally support `p50`, `p75`, `p90`, `p95`, and `p99`. A bounded two-term
equation supplies `operator` (`add`, `subtract`, `multiply`, or `divide`) and `right` with the same
shape as `left`. Nested expressions, raw filters, and arbitrary aggregate strings are not accepted.

### Alert actions

`status` is `triggered`, `high`, `medium`, or `resolved`. A destination action uses
`destination`; PagerDuty may also set `pagerDutySeverity` to `default`, `critical`, `warning`,
`error`, or `info`. An email action is either:

```yaml
email:
  target: suggested-assignees
```

or:

```yaml
email:
  target: team
  team: backend
```

Legacy metric alerts do not support `suggested-assignees`; use a team there.

Modern issue workflows may create a GitHub or GitHub Enterprise issue through a source-code
catalog owned by the referenced `SentryConnection`:

```yaml
actions:
  - status: triggered
    githubIssue:
      integration: company-github
      repository: checkout
      labels:
        - sentry
        - production
      assignee: backend-oncall
```

`integration` and `repository` are the readable aliases declared under
`SentryConnection.spec.integrations.sourceCode`; they are not Sentry numeric IDs. `labels` is
optional and bounded to 20 entries, and `assignee` is optional. This action is available only for
typed `issue` alerts with the `modern-26.10.1` profile. It is deliberately separate from Slack,
PagerDuty, and email delivery, and it is rejected before Sentry I/O for metric alerts, legacy
profiles, unknown aliases, or non-GitHub providers.

The operator persists Sentry's bounded issue-form metadata so the workflow UI exposes Repository,
Assignee, and Labels. A real integration can populate the async choices. The local acceptance
fixture proves the saved values through the workflow API and the visible form fields without
calling GitHub.

## `SentryProject`

The Kubernetes resource name is the stable Sentry project slug. Reconciliation is ensure-style:
the operator creates a missing project and continues with a recognized managed project. An
existing project without recognized ownership follows the referenced connection's
`projectManagement.unmanagedProjects` policy. Application manifests do not select separate create
or use modes.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: checkout
  namespace: payments
spec:
  sentry: company-sentry
  alertPolicy: backend-alerts
  team: payments
  platform: python
  displayName: Checkout API
  lifecycle:
    onDelete: Retain
  grantAccess:
    teams:
      - backend
```

### Project identity and lifecycle

| Path | Meaning |
| --- | --- |
| `spec.sentry` | Connection name. |
| `spec.alertPolicy` | Optional reusable policy name. |
| `spec.team` | Required Sentry team slug used for deterministic creation and project ownership. |
| `spec.platform` | Optional create-time Sentry SDK platform hint. |
| `spec.displayName` | Optional UI name; it does not change the stable slug. The slug is used when this field is omitted. |
| `spec.paused` | Suspends project mutation while preserving protected state. |
| `spec.lifecycle.onDelete` | `Retain` removes operator-owned children and releases ownership but keeps the project. `DeleteProject` guardedly deletes an operator-owned project. |

Connection-wide `ownership.role` and `ownership.revision` determine which operator installation may
mutate. `projectManagement.unmanagedProjects` determines whether an unmarked existing project is
rejected, observed in standby, or explicitly adopted. These platform controls are not duplicated
on each project.

`grantAccess.teams` belongs on each project, not on the connection. It grants additional teams
access to this project. User membership and team creation remain outside the operator.

### Client keys and DSN output

```yaml
spec:
  clientKeys:
    browser:
      active: true
      rotationRevision: 1
      publishDSN: true
      rateLimit:
        count: 100
        windowSeconds: 60
      loader:
        tracing: true
        replay: true
        logsAndMetrics: true
        userFeedback: true
        debug: false
```

Client-key map keys such as `browser` are arbitrary local aliases, not Sentry key types. Declaring
an entry creates and fully manages a new Sentry Client Key. Increasing `rotationRevision` creates a server-generated
replacement. Removing `rateLimit` disables the owned per-key limit. Loader booleans that are
omitted are unmanaged. `logsAndMetrics` and `userFeedback` require the modern profile.

For the normal single-key project, omit `clientKeys`: Sentry creates its default Client Key and the
operator automatically publishes that key's public DSN. With multiple keys, exactly one managed
entry may set `publishDSN: true`. The operator writes only the public DSN to the pre-created
`sentry-dsns` Secret under the `SentryProject` name and preserves all other keys and metadata.

To publish a Client Key that is managed outside the operator:

```yaml
spec:
  existingClientKey:
    name: Default
    publishDSN: true
```

The external key is selected by its exact readable Sentry name. It is never updated, rotated, or
deleted by the operator. External and operator-managed DSN publication are mutually exclusive. If
multiple active keys exist and no source is selected, reconciliation reports
`DSNClientKeyAmbiguous` instead of publishing an arbitrary credential.

### Project behavior and notification defaults

```yaml
settings:
  behavior:
    autoResolveHours: 24
    defaultEnvironment: production
    crashReportLimit: 5
    scrapeJavaScript: true
    allowedDomains:
      - https://app.example.com
      - https://*.example.net:443
    releaseAutoCreation: true
  notifications:
    subjectPrefix: "[Checkout]"
    subjectTemplate: "$project: $title"
    minimumDigestMinutes: 5
    maximumDigestMinutes: 30
```

`autoResolveHours: 0` disables auto-resolution. `crashReportLimit` uses `-1` for unlimited, `0` to
disable storage, or a positive limit. An empty `defaultEnvironment` clears the selection without
deleting an environment. `allowedDomains` owns a non-empty origin set; `*` allows every origin.
`releaseAutoCreation` is modern-only.

### Inbound filters

```yaml
settings:
  filters:
    browserExtensions: true
    localhost: true
    webCrawlers: true
    filteredTransactions: true
    legacyBrowsers: [ie, safari]
    hydrationErrors: true
    chunkLoadErrors: true
    ipAddresses: [192.0.2.0/24]
    releases: [legacy-*]
    errorMessages: ["*ResizeObserver loop limit exceeded*"]
    logMessages: ["*health-check-noise*"]
    applicationMetrics: ["internal.*"]
```

Legacy browser values are `ie`, `edge`, `safari`, `firefox`, `chrome`, `opera`, `android`, and
`opera_mini`. An explicit empty list disables every managed browser filter. `logMessages` and
`applicationMetrics` are modern-only.

### PII scrubbing

```yaml
settings:
  pii:
    scrubData: true
    scrubDefaults: true
    scrubIPAddresses: true
    sensitiveFields: [customer_token]
    safeFields: [request_id]
    advancedRules:
      - dataset: events
        type: email
        redaction: mask
        applications:
          - root: user
            path: [email]
```

Advanced detector types are `anything`, `creditcard`, `password`, `ip`, `imei`, `email`, `uuid`,
`pemkey`, `url_auth`, `us_ssn`, `userpath`, and `mac`. Redactions are `mask`, `remove`, `hash`, or
`replace`; `replace` may use `replacement`. Selector roots are `message`, `extra`, `request`,
`user`, `contexts`, `tags`, `breadcrumbs`, modern `log-body`, and modern `metric-attribute`.
Datasets are `events`, modern `logs`, or modern `application-metrics`.

### Grouping and symbolication

```yaml
settings:
  grouping:
    enhancements:
      - matcher: module
        pattern: third_party.*
        action: MarkOutOfApp
    fingerprints:
      - matcher: error.type
        pattern: PaymentError
        values: [payment-error]
        includeDefault: true
  symbolication:
    builtinSources: [microsoft, mozilla]
```

Enhancement matchers are `path`, `module`, `function`, or `package`; actions are `MarkInApp`,
`MarkOutOfApp`, `Include`, or `Exclude`. `negate` is optional. Fingerprint matchers are
`error.type`, `error.value`, `message`, `logger`, `level`, `stack.module`, `stack.abs_path`,
`stack.package`, or `stack.function`. Explicit empty rule lists clear the owned Sentry rule text.

Built-in symbol sources are `amd`, `autodesk`, `chromium`, `citrix`, `electron`, `intel`,
`microsoft`, `mozilla`, `nuget`, `nvidia`, `ubuntu`, and `unity`.

Custom `symbolSources` are keyed by local alias and contain exactly one of `http`, `s3`, or `gcs`:

```yaml
symbolSources:
  public-symbols:
    layout:
      type: native
      casing: default
    http:
      url: https://symbols.example.com/
      username: optional-user
      passwordSecretRef:
        name: sentry-project-inputs
        key: symbols-password
```

Layouts are `native`, `symstore`, `symstore_index2`, `ssqp`, `unified`, or `debuginfod`; casing is
`default`, `uppercase`, or `lowercase`. S3 requires `bucket`, AWS `region`, optional `prefix`, and
two Secret key references. GCS requires `bucket`, `clientEmail`, optional `prefix`, and a private-key
Secret reference. Every reference must point into the fixed `sentry-project-inputs` Secret.

### Team ownership and source mappings

```yaml
issueOwnership:
  fallthrough: AllMembers
  autoAssignment: IssueOwner
  codeownersAutoSync: false
  rules:
    - matcher:
        type: path
        pattern: src/checkout/*
      teams: [payments]
    - matcher:
        type: tag
        tagKey: component
        pattern: checkout
      teams: [backend]
codeMappings:
  checkout-source:
    integration: company-github
    repository: checkout
    stackRoot: /app
    sourceRoot: src
    defaultBranch: main
    importCodeowners: false
```

Ownership matcher types are `path`, `module`, `url`, and `tag`. Every referenced team must be the
primary team or appear in `grantAccess`. `fallthrough` is `AllMembers` or `NoOne`;
`autoAssignment` is `IssueOwner`, `SuspectCommits`, or `Disabled`.

Code mappings select aliases from `SentryConnection.spec.integrations.sourceCode`. Enabling
`importCodeowners` also owns the corresponding Sentry CODEOWNERS child.

### Project-local destinations and alerts

`SentryProject.spec.notificationTargets` and `spec.alerts` use exactly the same schema as a policy.
Use them for a project-specific rule; use `SentryAlertPolicy` for shared defaults. Project-local and
policy rule keys must not conflict.

## `SentryCronMonitor`

A Cron Monitor is an independently owned namespaced resource. `projectRef` must name a managed
`SentryProject` in the same namespace. The controller manages configuration; the application still
emits check-ins. Remote IDs and check-in history stay out of status.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryCronMonitor
metadata:
  name: checkout-settlement
  namespace: payments
spec:
  projectRef: checkout
  displayName: Checkout settlement
  crontab:
    expression: "0 */2 * * *"
    timezone: Europe/Nicosia
  checkInMarginMinutes: 10
  maxRuntimeMinutes: 30
  failureThreshold: 2
  recoveryThreshold: 1
  ownerTeam: payments
  enabled: true
  onDelete: Delete
```

Use `interval` instead of `crontab` for a bounded interval, for example
`interval: {value: 15, unit: minute}`. Units are `minute`, `hour`, `day`, `week`, `month`, and
`year`. A five-field crontab and an IANA timezone are required for the crontab form. Sentry creates
environments from application check-ins, so environment selection is deliberately not claimed as
monitor configuration. Sentry's public monitor API also has no description field.

`onDelete: Delete` verifies the protected remote identity and deletes it. `Retain` leaves the
remote monitor in Sentry and releases only protected operator state.

## `SentryUptimeMonitor`

Uptime uses the experimental Sentry 26 API and is rejected on the legacy compatibility profile.
The controller runs Sentry's preflight before create or update and verifies the saved object after
each mutation.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryUptimeMonitor
metadata:
  name: checkout-health
  namespace: payments
spec:
  projectRef: checkout
  displayName: Checkout health
  url: https://checkout.example.com/health
  method: GET
  intervalSeconds: 300
  timeoutMilliseconds: 5000
  failureThreshold: 3
  recoveryThreshold: 1
  environment: production
  ownerTeam: payments
  traceSampling: false
  responseCaptureEnabled: true
  assertions:
    - type: statusCode
      comparison: equals
      value: "200"
  enabled: true
  onDelete: Delete
```

Allowed intervals are 60, 300, 600, 1200, 1800, and 3600 seconds. Assertions are combined with
AND and support `statusCode`, `responseHeader`, and a bounded `jsonPath`, using `equals`,
`notEquals`, `lessThan`, or `greaterThan`.

Request header values and a body are never placed in the CR:

```yaml
headers:
  - name: X-Uptime-Probe
    valueFrom:
      name: sentry-uptime-inputs
      key: probe-token
bodyFrom:
  name: sentry-uptime-inputs
  key: request-body
```

Only `sentry-uptime-inputs` is readable. `Authorization`, `Proxy-Authorization`, `Cookie`, and
`Host` headers are rejected to prevent the controller from becoming a general Secret forwarding
primitive. URL userinfo, fragments, localhost names, `.local`/`.internal` names, and literal
private, loopback, link-local, multicast, or unspecified addresses are rejected. Sentry's checker
remains responsible for DNS re-resolution, redirects, and destination enforcement at request time;
the operator never connects to the monitored URL itself.

Sentry 26.10.1 does not render `responseCaptureEnabled` in its edit form. The controller still
round-trips and verifies the field through the public API; physical acceptance verifies every other
field in the browser and this one field through read-after-write API evidence.

## Compatibility summary

| Feature | `legacy-24.7.1` | `modern-26.10.1` |
| --- | --- | --- |
| Project lifecycle, teams, ownership, code mappings, client keys | Yes | Yes |
| Common filters, event PII, grouping, symbols, notification defaults | Yes | Yes |
| Typed issue and static/change metric alerts | Yes | Yes |
| Dynamic metric detection | No | Requires Seer support |
| `logs` metric preset | No | Yes |
| Bounded Application Metrics detector | No | Yes |
| Log/application-metric filters and PII datasets | No | Yes |
| Loader logs/metrics and user feedback | No | Yes |
| Release auto-creation | No | Yes |
| Legacy metric email to suggested assignees | No | Yes |
| Cron Monitor configuration | Yes | Yes |
| Uptime Monitor configuration | No | Yes |

Compatibility is validated before Sentry mutation. A field appearing in the Sentry UI is not a
guarantee that the self-hosted backend enabled the required service.

## Status and troubleshooting

Start with the top-level `Ready` condition and compare `metadata.generation` with
`status.observedGeneration`:

```sh
kubectl get sentryconnection company-sentry
kubectl get sentryalertpolicy backend-alerts -o yaml
kubectl -n payments get sentryproject checkout -o yaml
kubectl -n payments get sentrycronmonitor,sentryuptimemonitor
```

`SentryProject` also reports component conditions for the policy, project, DSN, client keys,
settings, access, ownership, destinations, alerts, code mappings, and symbol sources. Child maps in
status contain bounded state and reason values but never external IDs or credentials.

Common reasons:

- `ConnectionNotReady`: fix connection credentials, URL, organization, or transport first.
- `ComponentNotReady`: inspect the individual component conditions and child status maps.
- `AuthorizationChanged` or `ExternalObjectReplaced`: the protected external identity no longer
  matches; the operator fails closed instead of adopting a different object.
- `*RecoveryPending`: an interrupted mutation is ambiguous and requires the exact remote object to
  become observable; the operator does not blindly retry a create.
- `RolloutIncomplete`: at least one consuming project has not applied the policy generation.

Plain HTTP and skipped TLS verification are development exceptions, not production defaults.
