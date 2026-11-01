# Sentry Kubernetes Operator Helm chart

This alpha chart installs the Sentry Kubernetes Operator, its three CRDs, admission
webhooks, authenticated controller metrics endpoint, and protected-state namespaces. It is
generated from the repository's canonical Kustomize manifests.

The chart does not install Sentry, cert-manager, Gateway API, Prometheus Operator, provider
integrations, public DNS, certificates for public receivers, or credential Secrets.

## Support and prerequisites

- Kubernetes `>=1.35.0-0` and `<1.37.0-0`;
- Helm 3;
- cert-manager installed and Ready before the operator;
- one operator installation per cluster.

The resources use fixed cluster-scoped and namespace-scoped names, so changing the Helm
release name does not create an independent second installation. Do not install multiple
releases in one cluster.

The chart is alpha and is not published to a chart repository. Install it from the checked-
out source tree or a verified chart archive created by `make helm-package`.

## What the default installation creates

The default render contains:

- the `SentryConnection`, `SentryProject`, and `SentryAlertPolicy` CRDs;
- two controller replicas with leader election, admission webhooks, and cert-manager-owned
  certificates;
- authenticated TLS controller metrics and the metrics-reader role;
- five protected namespaces and their least-privilege RBAC boundaries;
- no Telegram registration workloads, public `TLSRoute`, Prometheus `ServiceMonitor`,
  `PrometheusRule`, or scraper binding.

Read the [configuration guide](../../docs/configuration.md) after installation for the CRD
resource model and admitted examples.

## Development install

Render and inspect the chart first:

```sh
helm lint charts/sentry-kubernetes-operator --kube-version 1.35.0
helm template sentry-operator charts/sentry-kubernetes-operator \
  --include-crds \
  --set image.repository=registry.example.com/sentry-kubernetes-operator \
  --set image.tag=dev
```

Install with a mutable development tag only when `production=false`:

```sh
helm upgrade --install sentry-operator charts/sentry-kubernetes-operator \
  --set image.repository=registry.example.com/sentry-kubernetes-operator \
  --set image.tag=dev
```

Wait for both replicas:

```sh
kubectl -n sentry-kubernetes-operator-system rollout status \
  deployment/sentry-kubernetes-operator-controller-manager --timeout=300s
```

## Production render

Production mode rejects a missing or malformed digest. It renders
`<repository>@sha256:<digest>` and ignores the tag for the workload image:

```yaml
production: true

image:
  repository: registry.example.com/sentry-kubernetes-operator
  tag: unused-in-production
  digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  pullPolicy: IfNotPresent
```

```sh
helm upgrade --install sentry-operator charts/sentry-kubernetes-operator \
  --values production-values.yaml
```

The chart validates digest syntax; it does not verify an image signature. Release operators
must verify the exact digest and release identity before installation. The repository's
`make deploy-production` path performs its separately documented cosign policy; see
[`docs/supply-chain.txt`](../../docs/supply-chain.txt).

## Values

| Value | Default | Description |
| --- | --- | --- |
| `production` | `false` | Require an immutable image digest. |
| `image.repository` | `example.com/sentry-kubernetes-operator` | Image repository without a tag or digest. |
| `image.tag` | `dev` | Development image tag used only when `image.digest` is empty. |
| `image.digest` | `""` | Optional canonical `sha256:` digest; required when `production=true`. |
| `image.pullPolicy` | `IfNotPresent` | Kubernetes image pull policy: `Always`, `IfNotPresent`, or `Never`. |
| `registration.enabled` | `false` | Render isolated Telegram registration, hook receiver/producer, and delivery-worker workloads. This does not enable alert-source wiring. |
| `registrationGateway.enabled` | `false` | Render two Gateway API v1 `TLSRoute` resources. Requires registration. |
| `registrationGateway.gatewayName` | `""` | Existing Gateway name. |
| `registrationGateway.gatewayNamespace` | `""` | Existing Gateway namespace. |
| `registrationGateway.listenerName` | `""` | Existing `TLS`/`Passthrough` listener section name. |
| `registrationGateway.telegramHostname` | `""` | Exact public Telegram receiver hostname. |
| `registrationGateway.sentryHostname` | `""` | Exact public Sentry hook receiver hostname; must differ from the Telegram hostname. |
| `registrationMonitoring.enabled` | `false` | Render the registration/delivery `ServiceMonitor` and `PrometheusRule`. Requires registration. |
| `controllerMonitoring.enabled` | `false` | Render the controller `ServiceMonitor` and `PrometheusRule`. |
| `controllerMonitoring.access.enabled` | `false` | Bind one exact Prometheus ServiceAccount to the controller metrics-reader role. Requires controller monitoring. |
| `controllerMonitoring.access.serviceAccountName` | `""` | Exact Prometheus ServiceAccount name. |
| `controllerMonitoring.access.serviceAccountNamespace` | `""` | Exact Prometheus ServiceAccount namespace. |

The JSON schema rejects unknown top-level and nested values. It also rejects monitoring or
Gateway options whose required parent feature is disabled.

## Opt-in registration components

Registration is disabled by default. Before enabling it, create the five administrator-
owned Secrets in `sentry-kubernetes-operator-system`:

| Secret | Required keys | Purpose |
| --- | --- | --- |
| `telegram-webhook-tls` | `tls.crt`, `tls.key` | Public Telegram receiver certificate. |
| `telegram-webhook-auth` | `secret-token` | Telegram webhook secret header. |
| `telegram-bot-auth` | `token` | Telegram Bot API credential. |
| `sentry-hook-receiver-tls` | `tls.crt`, `tls.key` | Public Sentry hook receiver certificate. |
| `sentry-hook-receiver-sources` | `<canonical-source-sha256>.json` entries | Fail-closed Sentry hook verifier configuration. The aggregate Secret may initially be empty. |

Secret values must be provisioned outside Git. The exact verifier JSON, rotation behavior,
and security boundary are documented in the repository [README](../../README.md) and
[`docs/telegram-design.txt`](../../docs/telegram-design.txt).

Enable the workloads with:

```yaml
registration:
  enabled: true
```

These components provide registration, receiver, queue, worker, and monitoring contracts.
They do not enable Telegram alert delivery: the current controller deliberately does not
map project event hooks to alert routes because those events do not identify a fired alert.

## Gateway API TLS passthrough

The optional routes require:

- Gateway API v1 with the Standard Channel `TLSRoute` kind installed;
- an existing Gateway whose named listener uses protocol `TLS`, mode `Passthrough`;
- a listener that permits routes from `sentry-kubernetes-operator-system`;
- two exact, distinct DNS hostnames;
- installation-owned public addresses, DNS records, and receiver certificates.

Example values:

```yaml
registration:
  enabled: true

registrationGateway:
  enabled: true
  gatewayName: public
  gatewayNamespace: gateway-system
  listenerName: tls
  telegramHostname: telegram.example.com
  sentryHostname: sentry.example.com
```

The chart schema validates names, hostnames, feature dependencies, and distinct hostnames.
Helm rendering cannot prove that the Gateway, listener, Services, DNS, or public address
exists. TLS passthrough preserves receiver-side certificate validation and hot reload.

## Monitoring

Both monitoring options require Prometheus Operator CRDs. Label the namespace containing
the Prometheus scraper with `metrics: enabled` so the shipped NetworkPolicy permits access.
Add any labels required by the installation's `ServiceMonitor` and `PrometheusRule`
selectors through an overlay or post-renderer.

Registration/delivery monitoring:

```yaml
registration:
  enabled: true
registrationMonitoring:
  enabled: true
```

It installs rules for oldest outstanding work, new dead letters, and queue-metrics refresh
failures. It does not configure an Alertmanager receiver.

Controller monitoring and exact scraper authorization:

```yaml
controllerMonitoring:
  enabled: true
  access:
    enabled: true
    serviceAccountName: prometheus
    serviceAccountNamespace: monitoring
```

The metrics endpoint is authenticated TLS. Enabling the `ServiceMonitor` alone does not
authorize a scraper. The optional access block creates one fixed-subject ClusterRoleBinding;
it never grants access to a namespace, group, or wildcard subject.

## CRD lifecycle and upgrades

Helm installs files under `crds/` before templated resources on the first installation. Helm
does not upgrade or delete those CRDs during an ordinary chart upgrade or uninstall.

Before upgrading the controller to a chart version with changed CRDs:

1. read the release compatibility notes and back up the installation as documented in
   [`docs/admin-recovery.txt`](../../docs/admin-recovery.txt);
2. apply the exact new CRDs from the verified chart source;
3. wait for the CRDs to be established;
4. run `helm upgrade` with the reviewed values and immutable image digest;
5. inspect controller rollout and CR conditions.

From a source checkout, the CRDs can be applied explicitly:

```sh
kubectl apply -f charts/sentry-kubernetes-operator/crds/
kubectl wait --for=condition=Established crd/sentryconnections.sentryconfiguration.io
kubectl wait --for=condition=Established crd/sentryprojects.sentryconfiguration.io
kubectl wait --for=condition=Established crd/sentryalertpolicies.sentryconfiguration.io
```

The alpha project does not claim hosted rollback evidence. Never roll back a controller
across an incompatible CRD/storage contract without the documented compatibility review.

## Uninstall and protected state

```sh
helm uninstall sentry-operator
```

Uninstall removes workloads and RBAC but intentionally preserves these five namespaces via
`helm.sh/resource-policy: keep`:

- `sentry-kubernetes-operator-state`;
- `sentry-kubernetes-operator-routing`;
- `sentry-kubernetes-operator-delivery`;
- `sentry-kubernetes-operator-hook-sources`;
- `sentry-kubernetes-operator-audit`.

They may contain ownership bindings, operation journals, routes, queue records, verifier
plans, or mutation audit records. Because uninstall removes their RBAC, back up and review
them before any deliberate manual deletion. Deleting them can make safe ownership recovery
impossible. Follow [`docs/admin-recovery.txt`](../../docs/admin-recovery.txt); do not edit
protected Secrets as a recovery shortcut.

The three CRDs also remain installed under Helm's CRD lifecycle. Delete them only as an
explicit, separately reviewed data-destruction action after all custom resources and
protected state have been handled.

## Maintainer workflow

Do not edit `files/` or `crds/` manually. They are generated from canonical Kustomize and
CRD sources:

```sh
make helm-generate
make verify-helm
```

`make verify-helm` checks generated drift, chart schema, both supported Kubernetes versions,
CRD inclusion, default and all-feature renders, negative value gates, production digest
rendering, and chart packaging.
