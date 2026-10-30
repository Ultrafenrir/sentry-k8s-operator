# Service-owner quick start

This path requires exactly one namespaced `SentryProject` from the service owner. It does
not install Sentry or create platform credentials.

## Platform prerequisites

Before the service owner starts, a platform administrator must have:

- installed the operator, CRDs, admission webhooks and cert-manager dependency;
- created a Ready `SentryConnection` with a valid API token;
- granted this namespace the readable team slug in `spec.access` with `allowCreate: true`;
- verified that the target Sentry installation supports managed project creation with
  default-rule suppression and an automatically available active client key.

The last capability is deliberately fail-closed. Without evidence from the target Sentry,
the resource reports `ManagedCreateUnsupported`; source fixtures are not live evidence.

## Create the project

Create the application namespace if the platform has not already provisioned it:

```sh
kubectl create namespace checkout-prod
```

Apply one resource. `team` is the readable Sentry team slug allowed by the platform policy;
there is no external identifier to look up or copy.

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: checkout
  namespace: checkout-prod
spec:
  project:
    create:
      team: payments
```

```sh
kubectl apply -f sentryproject.yaml
kubectl -n checkout-prod wait sentryproject/checkout \
  --for=condition=Ready --timeout=5m
kubectl -n checkout-prod get sentryproject/checkout
kubectl -n checkout-prod get secret checkout-sentry
```

Do not print or commit the Secret data. A managed project publishes the public DSN under
key `SENTRY_DSN` in `checkout-sentry` automatically; the CR does not need a `dsn` block.

## Start the workload only when the Secret exists

Reference the Secret as required input. Kubernetes keeps the container in
`CreateContainerConfigError` until the operator has published it.

```yaml
env:
  - name: SENTRY_DSN
    valueFrom:
      secretKeyRef:
        name: checkout-sentry
        key: SENTRY_DSN
        optional: false
```

The complete Deployment form is in
[`config/samples/workload_with_sentry_dsn.yaml`](../config/samples/workload_with_sentry_dsn.yaml).

## Diagnose without Sentry API knowledge

Use the published conditions; they do not require reading protected bindings or Sentry
response bodies:

```sh
kubectl -n checkout-prod describe sentryproject checkout
```

- `ConnectionNotReady`: the platform connection or credential reload is not ready.
- `AccessDenied`: the namespace/team is outside the platform-owned access rule.
- `ManagedCreateUnsupported`: the target installation has not proved the safe create
  capabilities; the platform administrator must resolve this.
- `ReferenceNotFound` or `ReferenceAmbiguous`: correct or uniquely name the readable team
  slug. Do not substitute an internal identifier.
- `CreationOutcomeUnknown`: an interrupted create cannot be proven safe to retry; follow
  the administrator recovery runbook rather than creating a second project.

The complete condition/action reference is in
[`docs/troubleshooting.md`](troubleshooting.md). The controller emits transition-only Events
from the same redacted conditions, but the current alpha does not claim a complete audit
trail, so status remains the source of truth.

Deleting this CR uses the default `Retain` policy: the Sentry project remains, while the
Kubernetes-owned DSN Secret is removed with the CR. External project deletion requires an
explicit guarded lifecycle choice and is intentionally not part of this quick start.

## Evidence boundary

`make verify-samples` validates this minimal shape through generated CRDs and live admission
webhooks on envtest Kubernetes 1.35/1.36. `make verify-t04-kind` proves the required Secret
startup behavior in kind. Project creation and DSN usability still require acceptance
against the actual Sentry installation and are not claimed by those local tests.
