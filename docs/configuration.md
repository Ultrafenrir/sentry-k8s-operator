# Configuration reference

This guide documents the complete public configuration surface of the three CRDs. The generated
CRDs remain the machine-readable source of validation constraints; this document explains how to
choose values and what the operator owns.

## Common rules

- `SentryConnection` and `SentryAlertPolicy` are cluster-scoped. `SentryProject` is namespaced.
- The default Helm chart installs the operator and one connection. Application teams own project
  and policy instances.
- An omitted optional field is unmanaged. For owned maps and lists, an explicitly empty value
  removes only children previously created and protected by this operator.
- Cross-field and Sentry-version checks run in the controller before external mutation. The CRDs
  intentionally avoid CEL validation so they can be installed on older Kubernetes API servers.
- Public DSNs are written only to the pre-created `sentry-dsns` Secret in the project namespace.
  The key is the `SentryProject` name. Optional symbol-source credentials are read only from the
  pre-created `sentry-project-inputs` Secret.
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

For a normal single-region installation, omit `ownership`. Use `Reject` unless there is a reviewed
migration requirement for existing Sentry projects.

## `SentryAlertPolicy`

A policy is a reusable alert template. Creating the policy does not call Sentry. A project opts in
with `spec.alertPolicy`, after which policy alerts and project-local alerts are merged by stable map
key. A conflicting key fails closed.

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
`throughput`, `duration`, `failure-rate`, `largest-contentful-paint`, and modern-only `logs`.
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

## `SentryProject`

The Kubernetes resource name is the stable Sentry project slug for a created project. The resource
may instead observe an existing project by slug, but read-only projects cannot own mutable children.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: checkout
  namespace: payments
spec:
  sentry: company-sentry
  alertPolicy: backend-alerts
  project:
    create:
      team: payments
      platform: python
      displayName: Checkout API
  role: Active
  lifecycle:
    onDelete: Retain
  grantAccess:
    teams:
      - backend
  dsn:
    managedClientKey: browser
```

To observe an existing project without creating it:

```yaml
spec:
  sentry: company-sentry
  project:
    use: existing-project-slug
  role: Standby
```

### Project identity and lifecycle

| Path | Meaning |
| --- | --- |
| `spec.sentry` | Connection name. |
| `spec.alertPolicy` | Optional reusable policy name. |
| `spec.project.create.team` | Primary Sentry team slug. |
| `spec.project.create.platform` | Optional Sentry platform identifier. |
| `spec.project.create.displayName` | Optional UI name; it does not change the stable slug. |
| `spec.project.use` | Existing project slug; mutually exclusive with `create`. |
| `spec.role` | `Active` requests management; `Standby` does not acquire ownership. |
| `spec.paused` | Suspends project mutation while preserving protected state. |
| `spec.lifecycle.onDelete` | `Retain` removes operator-owned children and releases ownership but keeps the project. `DeleteProject` guardedly deletes an operator-owned project. |

`grantAccess.teams` belongs on each project, not on the connection. It grants additional teams
access to this project. User membership and team creation remain outside the operator.

### Client keys and DSN output

```yaml
spec:
  clientKeys:
    browser:
      active: true
      rotationRevision: 1
      rateLimit:
        count: 100
        windowSeconds: 60
      loader:
        tracing: true
        replay: true
        logsAndMetrics: true
        userFeedback: true
        debug: false
  dsn:
    managedClientKey: browser
```

Client-key map keys are local aliases. Increasing `rotationRevision` creates a server-generated
replacement. Removing `rateLimit` disables the owned per-key limit. Loader booleans that are
omitted are unmanaged. `logsAndMetrics` and `userFeedback` require the modern profile.

`dsn.managedClientKey` selects a managed alias. `dsn.clientKey` selects an existing readable key
name instead. They are mutually exclusive. The operator writes only the public DSN to the
pre-created `sentry-dsns` Secret and preserves all other keys and metadata.

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

## Compatibility summary

| Feature | `legacy-24.7.1` | `modern-26.10.1` |
| --- | --- | --- |
| Project lifecycle, teams, ownership, code mappings, client keys | Yes | Yes |
| Common filters, event PII, grouping, symbols, notification defaults | Yes | Yes |
| Typed issue and static/change metric alerts | Yes | Yes |
| Dynamic metric detection | No | Requires Seer support |
| `logs` metric preset | No | Yes |
| Log/application-metric filters and PII datasets | No | Yes |
| Loader logs/metrics and user feedback | No | Yes |
| Release auto-creation | No | Yes |
| Legacy metric email to suggested assignees | No | Yes |

Compatibility is validated before Sentry mutation. A field appearing in the Sentry UI is not a
guarantee that the self-hosted backend enabled the required service.

## Status and troubleshooting

Start with the top-level `Ready` condition and compare `metadata.generation` with
`status.observedGeneration`:

```sh
kubectl get sentryconnection company-sentry
kubectl get sentryalertpolicy backend-alerts -o yaml
kubectl -n payments get sentryproject checkout -o yaml
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
