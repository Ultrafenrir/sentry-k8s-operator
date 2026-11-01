# Configuration guide

This guide is the user-facing reference for the `sentryconfiguration.io/v1alpha1` API.
It covers every configurable feature currently published by the three CRDs and points to
admitted examples. The generated CRD OpenAPI schemas remain authoritative for validation
limits and defaults.

The operator is alpha software. Configuration lifecycle acceptance does not imply event
ingestion or external notification delivery. The exact implementation and evidence boundary
is recorded in [`project-status.md`](project-status.md).

## Resource model

| Resource | Scope | Owner | Purpose |
| --- | --- | --- | --- |
| `SentryConnection` | Cluster | Platform administrator | Sentry endpoint, credentials, access policy, integration catalogs, compatibility profile, and regional ownership policy. |
| `SentryProject` | Namespace | Service owner | Managed or read-only project binding, DSN publication, settings, owned child resources, alerts, pause, and deletion policy. |
| `SentryAlertPolicy` | Cluster | Platform or shared-service owner | Reusable notification targets and alerts. A policy changes Sentry only when a `SentryProject` selects it. |

Public specs use readable names and aliases. Sentry numeric IDs, DSN key material,
integration credentials, Telegram chat IDs, ownership markers, and operation journals stay
out of public specs and status.

## Before applying configuration

The cluster must have the operator, CRDs, admission webhooks, and cert-manager dependency.
The platform administrator must create a `SentryConnection` before service owners create
projects. The source distribution can be installed with Kustomize or with the generated
[Helm chart](../charts/sentry-kubernetes-operator/README.md).

Connection credential Secrets always live in
`sentry-kubernetes-operator-system`. Project child Secrets, such as symbol-source
credentials, live in the same namespace as their `SentryProject`. Do not store secret
values in Git. For example:

```sh
read -s SENTRY_API_TOKEN
kubectl -n sentry-kubernetes-operator-system create secret generic company-sentry-api \
  --from-literal=token="$SENTRY_API_TOKEN"
unset SENTRY_API_TOKEN
```

For a private CA, create a separate Secret from a PEM file and reference its key:

```sh
kubectl -n sentry-kubernetes-operator-system create secret generic company-sentry-ca \
  --from-file=ca.crt=./company-sentry-ca.pem
```

## SentryConnection

The complete admitted example is
[`config/samples/sentryconfiguration_v1alpha1_sentryconnection.yaml`](../config/samples/sentryconfiguration_v1alpha1_sentryconnection.yaml).

### Top-level fields

| Field | Required | Meaning |
| --- | --- | --- |
| `spec.url` | Yes | Sentry base URL. HTTPS is required unless plain HTTP is explicitly allowed for local development. |
| `spec.organization` | Yes | Readable Sentry organization slug. |
| `spec.credentials.tokenSecretRef` | Yes | API token Secret name and key in the operator namespace. |
| `spec.credentials.caBundleSecretRef` | No | Private CA bundle Secret name and key in the operator namespace. Valid only with HTTPS. |
| `spec.transport` | No | Development-only HTTP or TLS verification exceptions. |
| `spec.access` | No | Namespace, project, team, and integration-alias authorization rules. |
| `spec.integrations` | No | Readable catalogs for installed Slack, PagerDuty, source-code, and registration-only Telegram integrations. Catalog entries never install provider integrations. |
| `spec.compatibility.alerts` | Required for alerts | Explicit `legacy-24.7.1` or `modern-26.10.1` adapter selection. It is never inferred from a server version. |
| `spec.ownership` | No | Regional `Active` or `Standby` role and monotonically increasing revision. |
| `spec.projectManagement.unmanagedProjects` | No | `Reject` by default; `Standby` observes an unmarked project; `Adopt` is allowed only on an Active connection. |

### Transport security

Verified HTTPS is the default. A private CA is the production-safe customization:

```yaml
spec:
  url: https://sentry.example.com
  credentials:
    tokenSecretRef:
      name: company-sentry-api
      key: token
    caBundleSecretRef:
      name: company-sentry-ca
      key: ca.crt
```

Local plain HTTP must be explicit:

```yaml
spec:
  url: http://host.docker.internal:26101
  transport:
    allowInsecureHTTP: true
```

`insecureSkipTLSVerify: true` keeps HTTPS encryption but disables certificate and hostname
verification. It is mutually exclusive with `caBundleSecretRef` and should not be used for
production credentials.

### Access rules

Each access rule has a stable `owner`, exact namespace names, and the permissions available
to those namespaces:

```yaml
spec:
  access:
    - owner: payments
      namespaces:
        matchNames: [checkout-prod]
      teams: [payments, platform-observability]
      allowCreate: true
      existingProjects:
        observe: [checkout-api]
        consumeDSN: [checkout-api]
      notificationTargets:
        slackWorkspaces: [company-slack]
        pagerDutyAccounts: [company-pagerduty]
        telegramTargets: [payments-alerts]
```

`observe` allows a read-only `project.use` binding. `consumeDSN` additionally permits DSN
publication for that existing project. `allowCreate` and `teams` authorize managed project
creation and team attachment. Notification policy grants catalog aliases, not provider IDs
or channel/service allowlists.

### Integration catalogs

Slack workspaces, PagerDuty accounts, source-code installations, and repositories are
declared as stable aliases for objects already installed or imported in Sentry:

```yaml
spec:
  integrations:
    slack:
      workspaces:
        - name: company-slack
          sentryName: Company Slack
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

Supported source-code providers are `github`, `github_enterprise`, `gitlab`, `bitbucket`,
and `vsts`.

The `integrations.telegram` block contains the bot-token reference, receiver URL, and
monotonic hook revision for the isolated registration/hook component contract. The current
project controller does not wire project events to Telegram alert routes because project
events do not preserve alert-trigger semantics. Telegram targets can be registered, but no
valid alert may reference `telegram/...`; see
[`telegram-design.txt`](telegram-design.txt).

```yaml
spec:
  integrations:
    telegram:
      botTokenSecretRef:
        name: telegram-bot-auth
        key: token
      receiverURL: https://sentry.example.com/events
      serviceHookRevision: 1
```

The bot-token Secret is loaded from `sentry-kubernetes-operator-system`. A larger
`serviceHookRevision` is reserved for the isolated rotation contract; it does not enable
current alert wiring.

### Regional ownership

```yaml
spec:
  ownership:
    region: eu-west-1
    role: Active
    revision: 1
  projectManagement:
    unmanagedProjects: Reject
```

`revision` is a platform-controlled fencing value. Promotion requires a larger revision;
an equal or stale revision cannot take ownership. Use the project-level handoff/release
workflow for normal moves. Do not use `Adopt` as a general takeover mechanism.

## SentryProject

Start with the minimal [`quickstart.md`](quickstart.md). The samples directory also contains
a read-only binding and a paused, full-surface example; see
[`config/samples/README.md`](../config/samples/README.md).

### Project selection and role

Exactly one selector is required:

```yaml
spec:
  project:
    create:
      team: payments
      platform: python
```

`project.create` manages a project whose Sentry slug is `metadata.name`. The optional
`platform` is passed on creation. A project found without an ownership marker follows the
connection's `unmanagedProjects` policy.

```yaml
spec:
  project:
    use: checkout-api
```

`project.use` is permanently read-only. It may observe the project and publish an authorized
DSN, but it forbids settings, issue ownership, code mappings, client keys, symbol sources,
team grants, notification targets, alerts, alert policies, and external project deletion.

`spec.role` defaults to `Active`. `Standby` can consume a protected binding and may own
separately authorized alerts, but it cannot mutate project settings, team grants, client
keys, symbol sources, code mappings, issue ownership, or project deletion. Regional
connection fencing still applies.

### Ownership semantics

Configuration components are intentionally narrow and independently reconciled:

- an omitted optional component is unmanaged;
- an explicitly present empty list or map clears only children proven to be owned by this
  exact `SentryProject` incarnation;
- unrelated Sentry objects are preserved;
- `spec.paused: true` pauses ordinary reconciliation, while deletion safety work still runs;
- conditions report component-specific progress, so one failed component does not erase
  successful state from another.

Review component-specific semantics before changing an omitted field to `{}` or `[]`.

### Top-level fields

| Field | Meaning |
| --- | --- |
| `spec.sentry` | Name of the cluster-scoped `SentryConnection`. |
| `spec.alertPolicy` | Optional cluster-scoped `SentryAlertPolicy` name. |
| `spec.project` | Managed `create` or read-only `use` selector. |
| `spec.role` | Requested `Active` or `Standby` role. |
| `spec.dsn` | Output Secret and client-key selection. |
| `spec.settings` | Typed filters, PII, behavior, grouping, and built-in symbol sources. |
| `spec.issueOwnership` | Ordered team-only issue ownership and assignment policy. |
| `spec.codeMappings` | Owned repository path mappings and optional CODEOWNERS import. |
| `spec.clientKeys` | Owned server-generated client keys, rate limits, and rotation. |
| `spec.symbolSources` | Owned Secret-backed HTTP, S3, and GCS symbol sources. |
| `spec.grantAccess` | Additional project teams authorized by the connection. |
| `spec.notificationTargets` | Readable Slack/PagerDuty selections and registration-only Telegram targets. |
| `spec.alerts` | Owned adapter-neutral alerts. Alert actions accept Slack and PagerDuty only. |
| `spec.paused` | Pause ordinary reconciliation without weakening deletion handling. |
| `spec.lifecycle.onDelete` | `Retain` by default or guarded `DeleteProject`. |

### DSN and managed client keys

A managed project without a `dsn` block publishes a public DSN to
`<metadata.name>-sentry` under key `SENTRY_DSN`. Customize the Secret name or select a
readable existing client key with `dsn.secret` and `dsn.clientKey`.

Managed client keys contain no caller-provided key material:

```yaml
spec:
  dsn:
    secret: checkout-runtime-sentry
    managedClientKey: primary
  clientKeys:
    primary:
      active: true
      rateLimit:
        count: 1000
        windowSeconds: 60
      rotationRevision: 1
```

Increasing `rotationRevision` creates a server-generated replacement and performs a guarded
DSN cutover. `dsn.managedClientKey` must name an active entry in `clientKeys`. Removing the
`clientKeys` field leaves keys unmanaged; setting `clientKeys: {}` removes only exact
operator-owned keys.

### Typed project settings

All settings are optional. A missing subfield is not changed.

| Component | Published fields |
| --- | --- |
| `settings.filters` | `browserExtensions`, `localhost`, `webCrawlers`, `filteredTransactions`, and the legacy browser set: `ie`, `edge`, `safari`, `firefox`, `chrome`, `opera`, `android`, `opera_mini`. |
| `settings.pii` | `scrubData`, `scrubDefaults`, `scrubIPAddresses`, `sensitiveFields`, `safeFields`, and typed `advancedRules`. |
| `settings.behavior` | `autoResolveHours`, `defaultEnvironment`, `crashReportLimit`, `scrapeJavaScript`, and `allowedDomains`. |
| `settings.grouping` | Ordered typed stack-frame `enhancements` and event `fingerprints`. |
| `settings.symbolication` | Complete managed set of portable built-in symbol sources. |

An admitted example of every settings family is in
[`sentryconfiguration_v1alpha1_sentryproject_features.yaml`](../config/samples/sentryconfiguration_v1alpha1_sentryproject_features.yaml).
Advanced PII exposes only built-in detector, redaction, and exact selector types; raw Relay
JSON and regular expressions are not accepted. Grouping exposes a portable typed subset;
raw Sentry rule text and grouping-profile migration are not accepted.

The portable built-in symbol-source values are `amd`, `autodesk`, `chromium`, `citrix`,
`electron`, `intel`, `microsoft`, `mozilla`, `nuget`, `nvidia`, `ubuntu`, and `unity`.
An explicit empty `builtinSources` set disables every managed built-in source.

### Custom symbol sources

`symbolSources` is a map keyed by a local DNS-label alias. Each entry selects exactly one of
`http`, `s3`, or `gcs` and a portable layout:

```yaml
spec:
  symbolSources:
    internal-http:
      layout:
        type: native
        casing: default
      http:
        url: https://symbols.example.invalid
        username: symbol-reader
        passwordSecretRef:
          name: checkout-symbols
          key: http-password
```

HTTP requires HTTPS. Supported layout types are `native`, `symstore`, `symstore_index2`,
`ssqp`, `unified`, and `debuginfod`; casing is `default`, `uppercase`, or `lowercase`.
S3 access/secret keys and the GCS private key must be Secret references in the project
namespace. Azure is not in the portable cross-version API.

### Team access and issue ownership

```yaml
spec:
  grantAccess:
    teams: [platform-observability]
  issueOwnership:
    rules:
      - matcher:
          type: path
          pattern: src/payments/*
        teams: [payments]
      - matcher:
          type: tag
          tagKey: service
          pattern: checkout
        teams: [platform-observability]
    fallthrough: AllMembers
    autoAssignment: IssueOwner
    codeownersAutoSync: true
```

Matchers are `path`, `module`, `url`, or `tag`. Fallthrough is `AllMembers` or `NoOne`.
Auto-assignment is `IssueOwner`, `SuspectCommits`, or `Disabled`. Every referenced team must
be the primary create team or appear in `grantAccess.teams`, and the connection access rule
must authorize it. Direct users, email owners, and raw ownership grammar are not accepted.

### Code mappings and CODEOWNERS

Code mappings reference only connection-owned source-code aliases:

```yaml
spec:
  codeMappings:
    checkout-source:
      integration: company-github
      repository: checkout
      stackRoot: /app
      sourceRoot: src
      defaultBranch: main
      importCodeowners: true
```

`importCodeowners: true` requires `defaultBranch`. It asks Sentry to fetch and own the
corresponding ProjectCodeOwners child; provider credentials and repository import remain
installation responsibilities. Numeric integration, repository, mapping, and CODEOWNERS
identities stay in protected state.

### Notification targets and alerts

Declare visible channels and services under their connection catalog aliases, then use the
provider-first canonical reference in each alert:

```yaml
spec:
  notificationTargets:
    slack:
      company-slack: [payments-alerts]
    pagerDuty:
      company-pagerduty: [Payments Primary]
  alerts:
    new-errors:
      trigger: first-seen
      notify: [slack/payments-alerts]
    error-volume:
      trigger: event-count
      windowMinutes: 5
      threshold: 200
      notify: [pagerDuty/Payments Primary]
```

Channel names must be unique across selected Slack workspaces, and service names must be
unique across selected PagerDuty accounts. Alerts never accept Sentry or provider IDs.

| Compatibility profile | Triggers | Enabled state |
| --- | --- | --- |
| `legacy-24.7.1` | `first-seen`, `regression`, `event-count`, `user-count` | Desired alerts must be enabled; deleting a legacy issue rule is not treated as pause. |
| `modern-26.10.1` | `event-count`, `user-count` | Enable and disable are supported. |

`event-count` and `user-count` require `windowMinutes` and `threshold`; issue triggers
forbid them. `notify` accepts only `slack/<channel>` and `pagerDuty/<service>`. Telegram
registration remains available as a separate component surface, but Telegram alert actions
are rejected until an authenticated alert-firing source can preserve trigger semantics.

A project may declare the readable registration target without referencing it from an
alert:

```yaml
spec:
  notificationTargets:
    telegram:
      - payments-alerts
```

An omitted `alerts` field leaves alerts unmanaged. `alerts: {}` deletes only protected
operator-owned alerts.

### Reusable SentryAlertPolicy

The complete sample is
[`config/samples/sentryconfiguration_v1alpha1_sentryalertpolicy.yaml`](../config/samples/sentryconfiguration_v1alpha1_sentryalertpolicy.yaml).

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryAlertPolicy
metadata:
  name: common-backend-alerts
spec:
  sentry: company-sentry
  notificationTargets:
    slack:
      company-slack: [backend-alerts]
  alerts:
    error-volume:
      trigger: event-count
      windowMinutes: 5
      threshold: 200
      notify: [slack/backend-alerts]
```

A project opts in with `spec.alertPolicy: common-backend-alerts`. Policy-owned and
project-owned alert or target keys must not conflict. Each alert may reference targets only
from its own scope. Policy status reports desired, ready, failed, and pending project counts
without listing consumer projects or protected identities.

### Pause and deletion

`spec.paused: true` stops ordinary external reconciliation. It is not a recovery or
authorization override, and deletion processing still takes precedence.

`lifecycle.onDelete: Retain` is the default. Retain removes operator-owned child
configuration, releases the verified remote ownership marker, and deletes protected local
state without deleting the Sentry project. `DeleteProject` is valid only for a managed,
Active project with current ownership and authorization evidence. Never use finalizer edits
or protected Secret edits as a deletion workaround; follow
[`admin-recovery.txt`](admin-recovery.txt).

## Status, Events, metrics, and audit

Use conditions as the primary diagnostic interface:

```sh
kubectl -n checkout-prod describe sentryproject checkout
kubectl get sentryconnection company-sentry
kubectl get sentryalertpolicy common-backend-alerts
```

Public status contains readable local names and fixed state/reason values. It does not
contain tokens, private DSNs, provider IDs, Sentry child IDs, protected hashes, or raw
external errors. Transition-only Kubernetes Events and bounded-label Prometheus metrics are
operational signals. The immutable mutation trail is exported separately with
`kubectl sentry audit-export`; see [`admin-recovery.txt`](admin-recovery.txt).

Condition-to-action guidance is in [`troubleshooting.md`](troubleshooting.md). Name
resolution, ambiguity, rename, and same-name replacement behavior is in
[`name-resolution.txt`](name-resolution.txt).

## Validate examples and changes

Every YAML file under `config/samples` is listed in its Kustomization and sent through the
real generated CRD schemas and admission webhooks:

```sh
make verify-samples
```

The samples use `.invalid` endpoints and Secret references without secret values. They are
API examples, not a deployable Sentry environment. Review names, policies, compatibility,
credentials, and lifecycle choices before applying any sample to a cluster.
