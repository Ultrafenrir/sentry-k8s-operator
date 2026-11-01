# Local development runbook

This is the canonical procedure for rebuilding the disposable local environment from zero,
running the lightweight Sentry stack, deploying the operator into kind, creating every
currently demonstrable CRD resource, and collecting honest evidence.

The commands assume the repository root as the working directory. They affect only the two
version-scoped Sentry Compose projects and the explicitly named kind cluster. They do not
prune Docker globally.

## Outcome

The default path creates:

- Sentry 26.10.1 with the `errors-only` profile on `http://127.0.0.1:26101`;
- organization `sko-live-26-10-1` and team `sko-live`;
- kind cluster `sentry-operator-dev` and context `kind-sentry-operator-dev`;
- two operator replicas with admission webhooks and authenticated TLS metrics;
- modern managed, existing-project, destination-surface, legacy-compatibility, and paused
  `SentryProject` examples;
- DSN Secrets and typed filter/PII settings; modern metric detector/workflow alerts become
  ready only when their referenced destination has a local catalog fixture.

The complete path normally takes 10-25 minutes after all images are cached. The upstream
Sentry installer dominates the duration.

## Prerequisites

Required tools are Docker Desktop, `git`, `make`, `curl`, `jq`, `kubectl`, Helm, and Bash
4.4 or newer. Repository-pinned Go, kind, controller-gen, and envtest tools live under
`.tools/` after the bootstrap targets have run.

Docker must expose at least 7 GiB and 2 CPUs for the lightweight 26.10.1 profile. The known
development machine exposes 15.6 GiB and 12 CPUs, which is sufficient for this path but not
comfortably sufficient for the 26.10.1 feature-complete profile.

Start with:

```sh
git status --short
make help
make verify-sentry-self-hosted
make docker-credential-diagnostics
docker info --format 'CPUs={{.NCPU}} Memory={{.MemTotal}}'
```

The diagnostic target reads only helper names from Docker configuration, requires every
configured helper executable to exist, and runs each helper's `list` operation with a bounded
timeout while discarding all helper output. Image build and push targets always run the static
preflight first. They do not print authentication entries, rewrite Docker configuration, or
bypass a missing, locked, failing, or hanging helper.

## 1. Remove only the managed local environment

The Sentry targets remove their own containers, networks, managed token files, and
version-scoped volumes. The kind command removes only the named project cluster.

```sh
make sentry-down SENTRY_VERSION=26.10.1 SENTRY_PROFILE=errors-only
make sentry-down SENTRY_VERSION=24.7.1 SENTRY_PROFILE=feature-complete
.tools/bin/kind delete cluster --name sentry-operator-dev
```

It is safe if a selected stack or cluster does not exist. Do not replace these commands with
`docker system prune`, `docker volume prune`, or deletion of every local cluster.

## 2. Start lightweight Sentry from empty volumes

```sh
make sentry-up \
  SENTRY_VERSION=26.10.1 \
  SENTRY_PROFILE=errors-only \
  SENTRY_WAIT_TIMEOUT=1800
make sentry-bootstrap SENTRY_VERSION=26.10.1 SENTRY_PROFILE=errors-only
make sentry-smoke SENTRY_VERSION=26.10.1 SENTRY_PROFILE=errors-only
```

If Docker Desktop hangs while invoking its credential helper and all required images are
already cached, rerun only the first command with `SENTRY_SKIP_PULL=true`. The wrapper
checks every rendered image before it bypasses upstream pulls.

Bootstrap creates a local superuser without a password and stores a scoped API token at
`.cache/sentry-self-hosted/tokens/26.10.1.token` with mode `0600`. The token must never be
printed, copied into documentation, or committed.

For UI inspection, assign a temporary password without placing it in shell history:

```sh
read -s SENTRY_LOCAL_PASSWORD
export SENTRY_LOCAL_PASSWORD
(cd .cache/sentry-self-hosted/26.10.1 && \
  docker compose --project-name sko-sentry-26-10-1 --profile errors-only \
  run --rm -T -e SENTRY_LOCAL_PASSWORD web shell -c \
  "import os; from sentry.users.models.user import User; u=User.objects.get(username='sko-local-acceptance@example.invalid'); u.set_password(os.environ['SENTRY_LOCAL_PASSWORD']); u.save(update_fields=['password'])")
unset SENTRY_LOCAL_PASSWORD
```

Log in with `sko-local-acceptance@example.invalid` and the temporary password. The password
is disposable; never add it to this repository. API verification does not require UI login.

## 3. Prove the Sentry configuration API before deploying the operator

```sh
make sentry-live-config-api SENTRY_VERSION=26.10.1
make sentry-controller-live SENTRY_VERSION=26.10.1
```

This destructive disposable-stack check covers the public project, client key, settings,
filter, modern detector, and workflow configuration lifecycles. It intentionally skips event
ingestion and notification delivery. The controller gate also exercises lost-response
recovery, a regional revision takeover with the real five-second fencing interval and
read-after-write, stale and conflicting writer refusal, protected child configuration drift
repair, and exact cleanup. The isolated service-hook lifecycle is not invoked by current
project reconciliation. This is configuration-only evidence, not a
physical multi-cluster or network-partition failover test.

## 4. Create kind and install cert-manager

```sh
make setup-test-e2e KIND_CLUSTER=sentry-operator-dev
kubectl config use-context kind-sentry-operator-dev
kubectl apply -f \
  https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=300s
kubectl -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=300s
kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s
```

Use the pinned repository kind binary. A system kind version can fail to load images into
this cluster even when cluster creation succeeds.

## 5. Build, load, and deploy the operator

```sh
make docker-build IMG=sentry-kubernetes-operator:dev
.tools/bin/kind load docker-image sentry-kubernetes-operator:dev \
  --name sentry-operator-dev
make deploy IMG=sentry-kubernetes-operator:dev
kubectl -n sentry-kubernetes-operator-system rollout status \
  deployment/sentry-kubernetes-operator-controller-manager --timeout=300s
kubectl -n sentry-kubernetes-operator-system get pods
```

The default deployment has two manager replicas. Both should be Ready. If an image build
hangs in Docker credential lookup, fix the Docker Desktop credential helper or use a clean,
task-scoped Docker configuration that preserves the active Docker endpoint. Do not edit the
operator image reference to an unrelated public image.

## 6. Publish the local Sentry token to the operator namespace

The recommended bounded workflow performs sections 6 and 7 without changing the current
kubectl context. It accepts only a `kind-*` context, reads the mode-0600 token only from the
ignored local-stack cache, applies the exact demo manifests, records the already proven local
capabilities, and waits only for components expected to become ready on a clean stack:

```sh
make kind-local-demo KIND_CONTEXT=kind-sentry-operator-dev
make kind-local-demo-status KIND_CONTEXT=kind-sentry-operator-dev
```

It is safe to rerun. It does not create Sentry, kind, cert-manager, the operator deployment,
or external integration credentials; complete sections 1 through 5, then run
`make sentry-bootstrap SENTRY_VERSION=26.10.1` and
`make sentry-live-config-api SENTRY_VERSION=26.10.1` first. To remove the demo, including only its
five named projects, two connections, credential Secret, and four ownership-labeled
namespaces, run:

```sh
make kind-local-demo-clean KIND_CONTEXT=kind-sentry-operator-dev
```

Cleanup refuses to delete a namespace when the exact demo ownership label is absent. The
commands below document the individual operations performed by the bounded workflow.

Create or replace the Secret directly from the protected token file. This command does not
print the token:

```sh
kubectl -n sentry-kubernetes-operator-system create secret generic demo-sentry-api \
  --from-file=token=.cache/sentry-self-hosted/tokens/26.10.1.token \
  --dry-run=client -o yaml | kubectl apply -f -
```

All `SentryConnection` credential references are resolved from the fixed operator namespace,
not from application namespaces.

## 7. Apply connection and project resources

```sh
kubectl apply -f config/demo/kind-local.yaml
kubectl wait sentryconnection/demo-modern \
  --for=condition=CredentialsReady --timeout=120s
kubectl wait sentryconnection/demo-legacy \
  --for=condition=CredentialsReady --timeout=120s
```

The connection controller deliberately loads credentials and transport configuration only;
it does not infer capabilities from a version string. A successful local
`sentry-live-config-api` or `sentry-live-api` run writes a mode-0600 capability evidence file
under `.cache/sentry-self-hosted/evidence/`. `kind-local-demo` validates its exact version,
organization, gate, and capability set before applying status to both demo connections.
Stack recreation or removal deletes the evidence file, so stale evidence cannot be reused.
Never synthesize or apply this local evidence to an unverified external Sentry installation.

Wait for the independently supported project, DSN, and settings components, then attach the
read-only consumer. The bootstrap creates the explicitly invalid local PagerDuty catalog
fixture used by the modern demo; it cannot prove notification delivery.

```sh
kubectl -n sentry-demo-modern wait sentryproject/modern-demo \
  --for=condition=ProjectReady --timeout=300s
kubectl -n sentry-demo-modern wait sentryproject/modern-demo \
  --for=condition=DSNReady --timeout=300s
kubectl -n sentry-demo-modern wait sentryproject/modern-demo \
  --for=condition=SettingsReady --timeout=300s
kubectl apply -f config/demo/kind-local-readonly.yaml
kubectl -n sentry-demo-modern wait sentryproject/readonly-demo \
  --for=condition=Ready --timeout=300s
```

## 8. Inspect the CRD surface and expected state

```sh
kubectl get sentryconnections
kubectl get sentryprojects -A
kubectl get sentryprojects -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,REASON:.status.conditions[?(@.type=="Ready")].reason'
kubectl -n sentry-demo-modern get secrets modern-demo-dsn readonly-demo-dsn
kubectl -n sentry-demo-modern describe sentryproject modern-demo
kubectl -n sentry-demo-surface describe sentryproject destination-surface
kubectl -n sentry-demo-legacy describe sentryproject legacy-demo
```

Expected state on Sentry 26.10.1 `errors-only`:

| Resource | Expected | Meaning |
| --- | --- | --- |
| `sentry-demo-modern/modern-demo` | `Ready=True` after bootstrap | Project, DSN, filters, PII, fake PagerDuty catalog pinning, and modern alert configuration reconcile on a clean stack. |
| `sentry-demo-modern/readonly-demo` | `Ready=True` | Existing project observed and its DSN published without taking project ownership. |
| `sentry-demo-surface/destination-surface` | Not fully Ready | PagerDuty is catalog-pinned by the invalid local fixture; Slack and Telegram intentionally remain unresolved without external integration/registration. |
| `sentry-demo-legacy/legacy-demo` | `AlertsReady=False` | The 26.10.1 server returns 404 for legacy alert collections. Use Sentry 24.7.1 for legacy acceptance. |
| `sentry-demo-paused/paused-demo` | Not Ready by design | `spec.paused: true` prevents mutation. |

`make sentry-bootstrap` creates the demo's disposable PagerDuty integration and service with
the literal invalid key `invalid-routing-key-not-for-delivery` and a synthetic numeric service
ID required by the Sentry workflow form. The helper is restricted to the disposable
version-scoped local organization. It proves catalog resolution and Sentry configuration
only, never PagerDuty delivery. Do not reproduce this fixture in production or place a real
routing key in this repository.

## 9. Verify the Sentry objects

Use the UI only as a secondary inspection surface. The API and Kubernetes status are the
repeatable evidence.

- Organization: `http://127.0.0.1:26101/settings/sko-live-26-10-1/`
- Projects: `http://127.0.0.1:26101/settings/sko-live-26-10-1/projects/`
- Modern project: `http://127.0.0.1:26101/settings/sko-live-26-10-1/projects/modern-demo/`
- Alerts: `http://127.0.0.1:26101/organizations/sko-live-26-10-1/alerts/rules/`

Confirm project names, client keys, inbound filters, PII settings, detector/workflow state,
and disabled alert state. Do not claim UI evidence when browser automation is unavailable;
record API and status output instead.

The bounded read-only API inventory provides the repeatable redacted view:

```sh
make sentry-api-report SENTRY_VERSION=26.10.1 > sentry-api-report.json
```

The output intentionally excludes DSNs, tokens, routing keys, external identities, raw Relay
configuration, and sensitive/safe field names.

Successful project reconciliation schedules a 30-second drift check. This bound is intended
for prompt repair of managed settings and other observable configuration without relying on
a Sentry webhook. A live acceptance run must change the remote value first, then record both
the incorrect read-back and the repaired read-back before claiming drift repair.

## 10. Inspect controller metrics

The default endpoint is authenticated TLS. A lightweight direct diagnostic can first verify
that metrics resources exist:

```sh
kubectl -n sentry-kubernetes-operator-system get service \
  sentry-kubernetes-operator-controller-manager-metrics-service
kubectl get clusterrole sentry-kubernetes-operator-metrics-reader
kubectl -n sentry-kubernetes-operator-system get pods \
  -l control-plane=controller-manager
```

For Prometheus discovery, install Prometheus Operator CRDs, label the scraper namespace
`metrics=enabled`, deploy `config/controller-monitoring`, and grant the exact Prometheus
ServiceAccount with `make deploy-controller-monitoring-access`. The supported product
metrics are:

- `sentry_operator_reconciles_total`;
- `sentry_operator_reconcile_duration_seconds`;
- `sentry_operator_condition_transitions_total`;
- `sentry_operator_sync_resources`.

Metrics use bounded controller, outcome, condition, status, synchronization mechanism, and
synchronization state labels. The fixed controller set includes `connection`, `project`, and
`alertpolicy`; policy rollout is represented by the `RolloutComplete` condition. Metrics
must not expose namespace, object name, Sentry identity, raw error, Secret, DSN, or
destination data.

## 11. Focused verification and handoff

```sh
make verify-sentry-self-hosted
make verify-english
make verify-t03 verify-t04 verify-t05 verify-t06-core verify-t07-core
git diff --check
git status --short
```

Record which Sentry profile was used and classify evidence precisely:

- configuration CRUD and reconciliation;
- event ingestion and filter behavior;
- notification delivery.

The default runbook proves only the first category.

## Troubleshooting

`CredentialsReady=False`: verify `demo-sentry-api` exists in
`sentry-kubernetes-operator-system`, contains non-empty key `token`, and Sentry is healthy.

`ManagedCreateUnsupported`: run the live configuration gate, then apply the explicit local
capability status patch from step 7. Do not infer capabilities from `serverVersion` alone.

`host.docker.internal` cannot be reached: verify Sentry is bound to `127.0.0.1:26101`, then
test from the kind node. Docker Desktop normally provides the host gateway name.

Legacy alert 404 on 26.10.1: expected. Start a clean 24.7.1 feature-complete stack and use
the legacy compatibility profile for that acceptance path.

Sentry installer or image build stalls: run `make docker-credential-diagnostics`, then inspect
Docker Desktop resource usage. Repair or unlock a helper reported as missing, failing, or
hanging. Use `SENTRY_SKIP_PULL=true` only when the wrapper confirms a complete cache; local
operator image builds and pushes never bypass Docker credential handling.

macOS `curl` rejects the controller metrics certificate with an empty issuer DN: this is a
local cert-manager/curl diagnostic limitation, not permission to disable TLS verification in
the shipped ServiceMonitor. Use the repository's kind monitoring gate for CA- and
hostname-verified evidence.

## Teardown

```sh
make undeploy ignore-not-found=true
make uninstall ignore-not-found=true
.tools/bin/kind delete cluster --name sentry-operator-dev
make sentry-down SENTRY_VERSION=26.10.1 SENTRY_PROFILE=errors-only
```

The operator intentionally preserves protected-state namespaces during undeploy. Review them
before deliberate deletion because they can contain ownership, routing, queue, and audit
state. The kind cluster deletion removes them as part of the disposable environment.
