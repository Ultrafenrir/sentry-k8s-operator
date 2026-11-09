# Configuration samples

These manifests demonstrate the public `sentryconfiguration.io/v1alpha1` API. They are
validated by `make verify-samples` against the generated CRD schemas.
They are not a ready-to-run environment: the URLs use the reserved `.invalid` domain and
the referenced Secrets contain no values in this repository.

| File | Purpose |
| --- | --- |
| `sentryconfiguration_v1alpha1_sentryconnection.yaml` | Platform connection, access policy, compatibility, regional ownership, and integration catalogs. |
| `sentryconfiguration_v1alpha1_sentryalertpolicy.yaml` | Reusable cluster-scoped alert policy. |
| `sentryconfiguration_v1alpha1_sentryproject.yaml` | Managed project with team access, Slack/PagerDuty targets, and all supported alert trigger families. |
| `sentryconfiguration_v1alpha1_sentryproject_features.yaml` | Paused full-surface project covering managed client keys, settings, symbol sources, issue ownership, code mappings, alert policy selection, pause, and lifecycle. |
| `sentryconfiguration_v1alpha1_sentryproject_existing.yaml` | Read-only existing-project binding with authorized DSN publication. |
| `workload_with_sentry_dsn.yaml` | Deployment that waits for the operator-owned public DSN Secret. |

The full-surface project is deliberately paused. Create its referenced namespace-local
Secrets and replace every example name before removing `paused: true`. Never commit API
tokens, symbol-source credentials, DSNs, or provider credentials.

For field semantics and focused snippets, read
[`docs/configuration.md`](../../docs/configuration.md). For the minimal service-owner path,
read [`docs/quickstart.md`](../../docs/quickstart.md).
