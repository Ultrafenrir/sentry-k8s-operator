# Sentry Kubernetes Operator

An alpha Kubernetes operator for declaring a Sentry project next to an application. The
public API uses readable project, team, Slack, PagerDuty, and Telegram names; external IDs,
DSNs, tokens, Telegram chat IDs, and operation journals stay out of CR specs and status.

The repository currently provides project ownership, declarative project-team access,
public DSN publication, typed filters/PII settings, destination pinning, legacy and modern alert adapters, protected
Telegram registration/delivery components, recovery tooling, metrics, and alpha packaging.
It is **not production-ready**: Slack runtime discovery, live external delivery, complete
upgrade recovery, SaaS acceptance, and hosted release evidence are still incomplete. See
[`docs/t06-destinations-progress.txt`](docs/t06-destinations-progress.txt),
[`docs/t07-alerts-progress.txt`](docs/t07-alerts-progress.txt), and
[`docs/kubernetes-compatibility.txt`](docs/kubernetes-compatibility.txt) for the precise
evidence boundary.

## Start here

Agents and maintainers must read [`AGENTS.md`](AGENTS.md) first. The canonical clean local
environment procedure is [`docs/development-runbook.md`](docs/development-runbook.md), and
the current implementation/evidence/backlog summary is
[`docs/project-status.md`](docs/project-status.md). These three files are intentionally
shorter and more current than the historical task reports.

All repository documentation and source comments must be written in English.

## Development

The checked-in toolchain targets Go 1.26 and Kubernetes 1.35/1.36 envtest. A persistent
development kind cluster can be created with:

```sh
make setup-test-e2e KIND_CLUSTER=sentry-operator-dev
kubectl --context kind-sentry-operator-dev get nodes
```

Run the generated-contract checks and both Kubernetes versions with:

```sh
make verify-t01-generated verify-t02 verify-t03 verify-t04 verify-t05 verify-t06-core verify-t08-core
make verify-samples
make test-matrix
```

Pinned local Sentry 24.7.1 and 26.10.1 stacks are also available through the official
Sentry Self-Hosted Compose trees. Every `sentry-up` removes the selected stack and all of
its persistent volumes, fetches a clean exact-tag checkout, runs the official installer,
and waits for the health endpoint:

```sh
make sentry-up SENTRY_VERSION=24.7.1
make sentry-smoke SENTRY_VERSION=24.7.1
make sentry-down SENTRY_VERSION=24.7.1

make sentry-up SENTRY_VERSION=26.10.1
make sentry-smoke SENTRY_VERSION=26.10.1
make sentry-down SENTRY_VERSION=26.10.1
```

Run only one version at a time. The default `feature-complete` profile needs at least
3.8 GB/2 CPUs for 24.7.1 and 14 GB/4 CPUs for 26.10.1. The explicitly selected
`SENTRY_PROFILE=errors-only` profile lowers the 26.10.1 requirement to 7 GB/2 CPUs, but
does not cover the complete alert/workflow acceptance surface. See
[`docs/self-hosted-test-stack.md`](docs/self-hosted-test-stack.md) for lifecycle,
isolation, CI, and current runtime-evidence details.

For the reproducible lightweight Sentry 26.10.1 + kind + operator + CRD demo, follow
[`docs/development-runbook.md`](docs/development-runbook.md) exactly. It includes cleanup,
capability evidence, expected partial resources, API/UI inspection, metrics, and teardown.

The registration RBAC probe uses the existing kind cluster and verifies that the writer
has only status/protected-state access while the public Telegram receiver has no
Kubernetes permissions:

```sh
make verify-t06-kind-rbac KIND_CONTEXT=kind-sentry-operator-dev
```

## Build and deploy the controller

Build and publish one distroless image containing the manager and the three opt-in
Telegram executables:

```sh
make docker-build docker-push IMG=<registry>/sentry-kubernetes-operator:<tag>
make install
make deploy IMG=<registry>/sentry-kubernetes-operator:<tag>
```

For a release, use `make docker-buildx` with a non-`latest` version tag; it pushes a
multi-platform image with SBOM and max provenance attestations and records the resulting
digest. Production deployment aliases accept only `repository@sha256:...`. See
[`docs/supply-chain.txt`](docs/supply-chain.txt); production aliases additionally require
cosign verification against an exact tagged release-workflow identity. The tag-only GitHub
Actions workflow builds, signs the pushed digest with keyless OIDC and immediately verifies
that exact identity. Its structure is source-tested; no hosted run, registry push or real
signature is claimed locally.

The source distribution also contains a generated alpha Helm chart. cert-manager must
already be installed. Development render/install example:

```sh
make verify-helm
helm upgrade --install sentry-operator charts/sentry-kubernetes-operator \
  --set image.repository=<registry>/sentry-kubernetes-operator \
  --set image.tag=<tag>
```

For production rendering set `production=true` and `image.digest=sha256:...`; a mutable
tag is then rejected by the values schema. Registration remains disabled unless explicitly
enabled. Helm uninstall preserves the five protected namespaces through resource-policy
annotations, but removes their RBAC; consult the recovery runbook before manual deletion.

`make deploy` creates separate `sentry-kubernetes-operator-state`,
`sentry-kubernetes-operator-routing`, `sentry-kubernetes-operator-delivery`, and
`sentry-kubernetes-operator-audit` namespaces plus the isolated
`sentry-kubernetes-operator-hook-sources` namespace before the controller. Bindings,
numeric routes, mutable queue records, authenticated hook source plans, and immutable
mutation audit records cannot share an RBAC boundary; platform credential Secrets stay in
`sentry-kubernetes-operator-system`.
Uninstall deliberately does not delete protected state automatically.

The sample manifests are API examples, not a working environment by themselves. CI sends
every rendered sample through the real CRD schema and admission webhooks with a server-side
dry-run. A real deployment still needs a `SentryConnection`, its referenced credential
Secrets, and a compatible Sentry installation:

```sh
kubectl apply -k config/samples/
```

For the service-owner path, start with the single-resource
[`docs/quickstart.md`](docs/quickstart.md). It separates platform prerequisites from the
minimal `SentryProject`, uses no external identifiers, and states which final checks still
require the actual Sentry installation.

Condition-driven diagnosis is documented in
[`docs/troubleshooting.md`](docs/troubleshooting.md); protected-state investigation stays
read-only through the redacted administrator report.

## Opt-in Telegram registration

Telegram registration and Sentry hook ingress are not part of the default deployment. They
require cert-manager, the controller/CRDs, and five administrator-provided Secrets in
`sentry-kubernetes-operator-system`:

- `telegram-webhook-tls` with a publicly trusted `tls.crt`/`tls.key` for the webhook;
- `telegram-webhook-auth` with `secret-token`, using only `A-Z`, `a-z`, `0-9`, `_`, or `-`;
- `telegram-bot-auth` with the Bot API credential in key `token`;
- `sentry-hook-receiver-tls` with the publicly trusted `tls.crt`/`tls.key` used by Sentry;
- `sentry-hook-receiver-sources`, whose keys are `<canonical-source-sha256>.json`. Each
  value is strict JSON
  `{"expectedProjectSlug":"checkout","hmacSecretsBase64":["<canonical-base64>"]}`.
  The array contains one active key or, during rotation, old and new keys; duplicates and
  more than two entries fail closed.

The aggregate source Secret may initially be empty. Receiver liveness remains healthy;
readiness fails only when a visible entry is malformed, so a bad rotation is removed from
traffic without a restart loop.

The last Secret is reloaded per request and must match a protected source-plan record. The
current alpha does not yet publish that pair from the controller; manual provisioning is
only suitable for controlled validation and must be kept in the encrypted backup boundary.

Then deploy the internal writer, both tokenless public receivers, internal hook producer,
and isolated delivery worker:

```sh
make deploy-registration IMG=<registry>/sentry-kubernetes-operator:<tag>
```

The installation must still provide public DNS and call Telegram `setWebhook` with the
same secret token. For a Gateway API implementation that supports the Standard Channel
`TLSRoute` feature, attach both receivers to an existing `TLS`/`Passthrough` listener:

```sh
make deploy-registration-gateway \
  GATEWAY_NAME=public \
  GATEWAY_NAMESPACE=gateway-system \
  GATEWAY_LISTENER=tls \
  TELEGRAM_HOSTNAME=telegram.example.com \
  SENTRY_HOSTNAME=sentry.example.com
```

The listener must allow routes from `sentry-kubernetes-operator-system`. The target checks
that `gateway.networking.k8s.io/v1` is served, the named listener is exactly
`TLS`/`Passthrough`, both Services exist, and both resources pass server-side dry-run. It
then waits until the Gateway controller reports `Accepted=True` and `ResolvedRefs=True` for
each route. Exact, distinct FQDNs are required so SNI selects only one backend. The Gateway
implementation, `GatewayClass`, public address, listener, DNS records, and certificates are
deliberately not created by this repository. Passthrough keeps TLS termination and
certificate hot reload in the receiver pods. Use `undeploy-registration-gateway` with the
same five values to remove only these two routes.

The same opt-in is available in Helm through `registrationGateway.enabled` and its five
required values. It is rejected unless `registration.enabled=true`; it remains disabled in
the default chart render.

Repository maintainers can reproduce the API-only and real dataplane checks without Sentry
or Telegram credentials:

```sh
make verify-t08-kind-gateway-api
make verify-t08-kind-gateway-dataplane
```

Both targets refuse to reuse an existing cluster name and delete only the kind cluster they
created. The dataplane gate uses pinned Envoy Gateway/controller images and verifies the
receiver-owned certificate over SNI/TLS passthrough; it does not prove public DNS or an
internet-routable address.

The Telegram receiver accepts Telegram on service port 443
and can egress only to DNS and the mTLS writer. The writer is internal-only and has no
Sentry token. The tokenless Sentry receiver reads projected source configuration per
request and hands a closed two-hash envelope to the internal mTLS producer; only that
producer can create queue records. The delivery worker reloads the bot token per operation
and has no Sentry credentials. Deploying these workloads does not enable alert delivery
until the protected source plan and matching public verifier config are provisioned.
It exposes identity-free Prometheus metrics on its internal metrics Service. Only pods in
namespaces labeled `metrics: enabled` are admitted by the registration NetworkPolicy;
no ServiceMonitor or alert rules are installed automatically.

If Prometheus Operator CRDs are installed, label the namespace containing the Prometheus
scraper with `metrics: enabled`, add any labels required by that installation's monitor/rule
selectors through an overlay, and install the optional defaults:

```sh
make deploy-registration-monitoring
```

The default rules alert on oldest outstanding age, new dead letters, and queue-metrics
refresh failures. They contain no project, route, chat, record, or payload labels.
Repository maintainers can reproduce real ServiceMonitor/PrometheusRule discovery in an
isolated kind cluster with `make verify-t08-kind-monitoring`; the gate scrapes both worker
replicas, loads all three shipped alerts and fires a disposable rule on a product metric.
It does not configure a production Alertmanager receiver.

Controller product metrics are also opt-in at the Prometheus discovery layer. The
endpoint itself is always authenticated TLS in the default deployment. If Prometheus
Operator CRDs are installed, label the scraper namespace `metrics: enabled`, add any
installation-specific selector labels through an overlay, and install the CA-verifying
ServiceMonitor plus fixed-label alert rules:

```sh
make deploy-controller-monitoring
make deploy-controller-monitoring-access \
  PROMETHEUS_SERVICE_ACCOUNT_NAME=prometheus \
  PROMETHEUS_SERVICE_ACCOUNT_NAMESPACE=monitoring
```

The rules alert on reconcile errors and bursts of failed condition transitions. They use
only fixed controller/condition/outcome labels; object names, namespaces, Sentry IDs and
error text are excluded. The separate access target verifies that the exact ServiceAccount
and `sentry-kubernetes-operator-metrics-reader` ClusterRole exist, performs server-side
dry-run, and creates one fixed-subject ClusterRoleBinding. It never grants access to a
namespace, group, or wildcard subject. Remove it with `undeploy-controller-monitoring-access`
using the same values.

Helm users can enable discovery with `controllerMonitoring.enabled=true` and the exact
binding with `controllerMonitoring.access.enabled=true`,
`serviceAccountName`, and `serviceAccountNamespace`. Access remains disabled by default and
the chart rejects it when controller monitoring or either exact identity value is absent.
`make verify-t09-kind-monitoring-access` reproduces the least-privilege authorization check
against the configured development kind cluster and removes its temporary binding and
namespace afterward. `make verify-t09-kind-controller-monitoring` runs the full contract in
a disposable Kubernetes 1.35.5 cluster: both controller targets must first fail closed with
HTTP 403, then recover through that exact binding, expose product metrics over CA- and
hostname-verified TLS, load both shipped alerts, and fire a disposable rule. The probe
installs pinned cert-manager and Prometheus Operator releases and deletes only its own
cluster afterward.

Build the registration client plugin and put it on `PATH` as `kubectl-sentry`:

```sh
make build-kubectl-sentry
install -m 0755 bin/kubectl-sentry <directory-on-PATH>/kubectl-sentry
kubectl sentry telegram-register \
  --endpoint https://<registration-endpoint> \
  --ca-file <registration-ca.pem> \
  --namespace <namespace> \
  <project> <destination>
```

The endpoint requires a bearer token accepted for the configured
`sentry-registration` audience and performs an exact-name SubjectAccessReview for `update`
on the selected `SentryProject`. Use `--token-file` when the current kubeconfig credential
does not carry that audience. The plugin prints the one-time `/register ...` command only;
it does not persist it or include server error bodies in output.

## Read-only recovery inventory

An administrator can inspect protected recovery state without printing external IDs, DSNs,
Telegram targets, message payloads, operation keys, hashes, or raw Secret data:

```sh
kubectl sentry admin-report --context <context>
```

The command performs no writes. Its identity needs `list` on Secrets only in the state,
hook-source, routing, and delivery namespaces. It reports local object names, fixed
state-machine phases, aggregate hook-source, alert/tombstone, and queue/expiry counts. See
[`docs/admin-recovery.txt`](docs/admin-recovery.txt) before using the report for recovery;
the current command deliberately provides no destructive break-glass action.

An administrator with the separately bindable
`sentry-kubernetes-operator-protected-audit-reader` Role can export the validated,
identity-free mutation trail in deterministic event-time order:

```sh
kubectl sentry audit-export --context <context> > mutation-audit.json
```

The command fails without partial output if any audit record is malformed. It never prints
Secret names, UIDs, resource versions, external identities, request payloads, or raw errors.
Audit records are immutable, and a fail-closed ValidatingAdmissionPolicy also denies every
UPDATE to an existing record; a leader-only bounded janitor retains 30 days by default.

## Removal

```sh
make undeploy-registration ignore-not-found=true
make undeploy ignore-not-found=true
make uninstall ignore-not-found=true
```

These commands preserve all five protected namespaces. Review and back up their contents
before deliberate deletion; deleting them can destroy ownership, alert pinning/journals,
routing, dedupe, DLQ, or mutation audit evidence.

## Design and compatibility

- [`SENTRY_CONFIGURATION_OPERATOR_PLAN_V2.txt`](SENTRY_CONFIGURATION_OPERATOR_PLAN_V2.txt)
  defines product intent and sequencing.
- [`docs/project-status.md`](docs/project-status.md) is the current implementation,
  evidence, deferral, and backlog source of truth.
- [`docs/alpha-surface.txt`](docs/alpha-surface.txt) records the published alpha boundary.
- [`docs/name-resolution.txt`](docs/name-resolution.txt) defines readable-name resolution
  and pinning.
- [`docs/telegram-design.txt`](docs/telegram-design.txt) separates registration and
  delivery security.
- [`docs/t08-telegram-delivery-progress.txt`](docs/t08-telegram-delivery-progress.txt)
  records the durable queue/worker evidence boundary.
- [`docs/t09-lifecycle-progress.txt`](docs/t09-lifecycle-progress.txt) records the current
  HA/recovery evidence and the remaining production work.
- [`docs/admin-recovery.txt`](docs/admin-recovery.txt) defines the redacted recovery
  inventory, least-privilege access, and current break-glass boundary.
- [`docs/supply-chain.txt`](docs/supply-chain.txt) defines attested release builds,
  signed immutable deployment verification, and the remaining signing-workflow boundary.
- [`docs/t10-packaging-progress.txt`](docs/t10-packaging-progress.txt) records generated
  Helm chart guarantees and the remaining end-to-end release work.

## License

Copyright 2026 Sentry Kubernetes Operator Authors. Licensed under the Apache License 2.0.
