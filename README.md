# Sentry Kubernetes Operator

Sentry configuration as code for Kubernetes.

Sentry Kubernetes Operator provides declarative, GitOps-ready automation for Sentry projects,
alerts, Client Keys, Cron Monitors, and Uptime Monitors. Platform teams configure one shared Sentry
connection; application teams keep their Sentry configuration beside their workloads as Kubernetes
custom resources. The controller continuously verifies remote state, repairs supported drift, and
uses protected ownership evidence before changing or deleting Sentry objects.

The project supports Sentry SaaS-compatible APIs and version-pinned self-hosted Sentry profiles.
It is currently alpha: the API may still change, and a compatibility claim requires repeatable
tests against the named Sentry and Kubernetes versions.

## Acceptance-tested targets

The supported product surface has been exercised through Sentry's browser UI and API, Kubernetes
resource status, and controller logs. Acceptance includes create, update, guarded adoption,
retention and deletion, drift verification, and two-replica controller restarts.

| Sentry target | Acceptance coverage |
| --- | --- |
| Sentry SaaS | Tested against the live service on 2026-11-19 in an isolated disposable organization. Project, issue-workflow, Client Key/DSN, Cron, Uptime, adoption, policy cleanup, and lifecycle paths were verified. SaaS API limitations are recorded in the acceptance report. |
| Self-hosted 26.10.1 | Tested with the version-pinned feature-complete profile, including the complete managed resource graph, modern issue and metric workflows, Cron, Uptime, Application Metrics, installed-destination resolution, restart recovery, and lifecycle cleanup. |
| Self-hosted 24.7.1 | Tested with the version-pinned legacy profile, including project configuration, legacy alerts, Client Keys/DSN, access, Cron, adoption, restart recovery, and lifecycle cleanup. Uptime is rejected before mutation because this profile does not support the modern Uptime API. |

These are tested compatibility targets, not a claim that every Sentry release is supported. See
the [acceptance report](REPORT), [current project status](docs/project-status.md), and
[acceptance runbook](docs/acceptance-runbook.md) for evidence, known limitations, and the exact
release checklist.

## What it manages

- `SentryConnection` selects one Sentry organization, credentials, compatibility profile,
  integration catalogs, adoption policy, and optional multi-operator fencing.
- `SentryProject` ensures a project exists, then manages selected settings, team access, Client
  Keys, public DSN publication, ownership rules, code mappings, symbol sources, and project-local
  alerts.
- `SentryAlertPolicy` defines reusable typed alerts that projects consume without exposing raw
  Sentry queries or provider payloads.
- `SentryCronMonitor` manages Cron Monitor configuration while applications continue to emit
  check-ins.
- `SentryUptimeMonitor` manages bounded Sentry 26 Uptime Monitor configuration.

The default Helm release installs the CRDs, one controller, HTTP metrics, protected controller
state, RBAC, and one `SentryConnection`. Application teams create project, policy, and monitor
resources in their own deployment repositories.

## What it does not manage

The operator does not install Sentry, ingest application events or check-ins, deliver Slack or
PagerDuty notifications, create users or teams, install third-party integrations, or accept
arbitrary Sentry query and integration payloads. It never stores API tokens, secret keys, private
DSNs, or protected remote identities in custom resources or public status.

## Project example

The Kubernetes resource name is the Sentry project slug. The operator looks up that slug first: it
creates a missing project and continues managing a project already protected by the same operator.
An existing unmarked project follows the `SentryConnection.spec.projectManagement` policy.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: checkout
  namespace: payments
spec:
  sentry: company-sentry
  team: payments
  platform: python
  displayName: Checkout API
  lifecycle:
    onDelete: Retain
```

`team` is required because deterministic Sentry project creation is team-scoped. `platform` is an
optional create-time SDK hint but is included in the quickstart so newly created projects have
useful onboarding metadata. `displayName` is optional and defaults to the project slug.

Sentry creates one default Client Key for a new project. When it is the only active key, the
operator automatically publishes its public DSN to the pre-created `sentry-dsns` Secret under the
key `checkout`. Advanced users can declare operator-owned Client Keys for independent rotation,
rate limits, or loader settings and mark exactly one with `publishDSN: true`.

## Install with Helm

Create a values file for the target Sentry organization. This example deploys
`ultrafenrir/sentry-k8s-operator:v0.1.0`:

```yaml
image:
  repository: ultrafenrir/sentry-k8s-operator
  tag: v0.1.0
  pullPolicy: IfNotPresent

sentryConnection:
  name: company-sentry
  spec:
    url: https://sentry.example.com
    organization: company
    credentials:
      tokenSecretRef:
        name: sentry-api-token
        key: token
    compatibility:
      alerts: modern-26.10.1
    projectManagement:
      unmanagedProjects: Reject

controllerMonitoring:
  enabled: false
```

Create the token Secret in the release namespace, then install the chart:

```sh
export OPERATOR_NAMESPACE=observability

kubectl create namespace "$OPERATOR_NAMESPACE"
kubectl -n "$OPERATOR_NAMESPACE" create secret generic sentry-api-token \
  --from-literal=token='YOUR_SENTRY_API_TOKEN'

helm upgrade --install sentry-operator ./charts/sentry-kubernetes-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --create-namespace \
  -f values-production.yaml
```

Application namespaces pre-create the fixed DSN output Secret:

```sh
kubectl -n payments create secret generic sentry-dsns
```

The controller can read and patch only Secrets named `sentry-dsns` outside its own namespace; it
cannot create, delete, list, or watch application Secrets. Optional project inputs and Uptime
request values use the separately pre-created, read-only `sentry-project-inputs` and
`sentry-uptime-inputs` Secrets.

## Ownership and adoption

`SentryConnection.spec.ownership` coordinates multiple operator installations. `Active` may
mutate, `Standby` observes without taking ownership, and a monotonically increasing `revision`
fences stale writers. `spec.projectManagement.unmanagedProjects` independently controls what an
active installation does when the requested slug already exists without recognized ownership:
`Reject`, `Standby`, or guarded `Adopt`.

## Documentation

- [Quickstart](docs/quickstart.md)
- [Complete CRD configuration reference](docs/configuration.md)
- [Physical acceptance runbook](docs/acceptance-runbook.md)
- [Upgrade and rollback policy](docs/upgrades.md)
- [Development runbook](docs/development-runbook.md)
- [Current project status](docs/project-status.md)

## Development

```sh
make help
make generate manifests
make test
make lint
make verify-english
git diff --check
```
