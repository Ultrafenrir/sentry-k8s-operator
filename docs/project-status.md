# Project status and continuation guide

Last reviewed: 2026-10-31.

This document is the short, current source of truth for continuation. Detailed historical
evidence remains in the task progress files and Git history. Update this file whenever a
feature crosses an evidence boundary or the prioritized backlog changes.

## Current alpha scope

The operator provides:

- cluster-scoped `SentryConnection` with fixed-namespace Secret input, explicit HTTP or TLS
  transport policy, access policy, and explicit alert compatibility profile;
- namespaced `SentryProject` create-or-use lifecycle with guarded ownership, protected
  operation journals, default-rule suppression, active DSN publication, drift repair, and
  Retain/DeleteProject deletion policy;
- typed inbound filters and PII settings;
- readable Slack, PagerDuty, and Telegram notification targets with protected identity
  pinning and fail-closed re-resolution;
- cluster-scoped self-service `SentryAlertPolicy` resources, project opt-in by name,
  provider-first targets, protected per-child ownership, rollout status, and guarded
  deletion;
- legacy Sentry 24.7.1 issue/metric alert adapters and modern Sentry 26.10.1 metric
  detector/workflow adapters;
- protected Telegram registration, hook verification, durable at-least-once queue, retry,
  dead-letter state, and isolated worker components;
- transition-only Kubernetes Events and bounded-label Prometheus controller metrics;
- HA-oriented deployment, protected-state namespaces, read-only recovery inventory, audit
  export, Kustomize resources, and an alpha Helm chart.

The project is not production-ready. The public API is `v1alpha1`, several end-to-end
external integrations remain unverified, and upgrade/release operations still need hosted
evidence.

## Live local evidence

Verified against disposable self-hosted Sentry:

| Target | Verified boundary |
| --- | --- |
| Sentry 24.7.1 feature-complete | Project/key/settings CRUD, legacy issue and metric alert CRUD, controller lost-response recovery, and settings drift repair. Event ingestion was blocked by Relay memory health on the constrained Docker VM. |
| Sentry 26.10.1 errors-only | Project/key/settings CRUD, modern metric detector/workflow lifecycle, controller lost-response recovery, and settings drift repair. This is configuration-only evidence. |

Sentry 26.10.1 returns 404 for the legacy `/rules/` and `/alert-rules/` collection routes.
Legacy compatibility must remain not ready on that server.

The latest local kind demo proved modern managed-project creation, an existing-project
read-only consumer, DSN publication, filters/PII settings, modern alerts, PagerDuty catalog
pinning with a fake local service fixture, and controller metrics presence. Browser
automation was unavailable, so no automated UI claim was made.

## Expected incomplete local resources

- Slack runtime channel discovery is fail-closed because one version-compatible public
  channel catalog is not proven across both supported Sentry versions.
- Telegram destinations require registration and real bot/chat/topic data.
- PagerDuty local catalog fixtures do not prove delivery.
- Paused projects intentionally do not mutate Sentry.
- Legacy alert resources do not become alert-ready on Sentry 26.10.1.

## Explicitly deferred by the repository owner

Do not spend time on these items unless the owner reopens them:

- hosted registry push, cosign signing/verification evidence, Helm publication, and hosted
  release workflow execution;
- human quick-start usability testing;
- SaaS compatibility and least-privilege token-scope acceptance;
- real Slack/PagerDuty notification delivery and external duplicate/revoke/cleanup
  acceptance;
- real Telegram send/retry/dead-letter acceptance with bot/chat/topic credentials;
- public DNS, publicly trusted certificates, and verification from an external network;
- production Alertmanager notification routing.

Configuration creation inside Sentry remains in scope even when external delivery is
deferred.

## Next in-scope work

### Ownership evolution in progress

The v1alpha1 types now include platform-owned connection `ownership` (`region`,
`Active|Standby`, and positive `revision`), `projectManagement.unmanagedProjects` with
`Reject` as the default, project acquisition role, bounded role status, stable Slack
workspace and PagerDuty account catalog aliases, and `notificationTargets`. The clean alpha
API requires `spec.sentry` and does not retain the old connection/default/destinations
shapes. Catalog authorization grants an integration alias and deliberately does not
maintain channel or service allowlists.

`internal/ownership` implements and fake-clock tests the connection-wide revision decision,
equal-revision conflict, stale-revision fencing, and write-delay-read takeover sequence.
This is a domain foundation, not yet controller or live Sentry evidence. The controller does
not yet persist the regional marker, revalidate it around every external mutation, promote
standby project candidates, or perform adoption. Until that integration is complete, the
new ownership fields must not be described as live failover support.

The reusable alert-policy design decision is complete: `SentryAlertPolicy` is cluster
scoped, ordinary cluster RBAC controls policy CRUD, and any project may opt in. There is no
policy owner field or custom authorization webhook. Policy and project targets use separate
ownership scopes, exact UIDs are persisted in protected bindings, updates enqueue all
consumers, and a finalizer holds deletion until policy-owned children are gone. Runtime and
generated-contract verification pass in unit and envtest suites. No policy rollout has been
exercised against live Sentry, so this is not live configuration or delivery evidence.

1. Implement the self-service ownership and regional failover contract without changing the
   external project slug. A connection declares a human-readable `region`, Active/Standby
   `role`, and monotonically increasing `revision`. Leader election coordinates replicas;
   protected claims coordinate competing namespaced CRs; the exact remote ownership marker
   and revision coordinate regions. One CR manages project settings/lifecycle, authorized
   standby CRs may consume DSNs and contribute separately owned alerts, and conflicting
   ownership fails closed. Existing unmarked projects follow connection-owned
   Reject/Standby/Adopt policy with Reject as the default. Regional takeover is explicit,
   connection-wide, audited, fenced by a larger revision, revalidated before every external
   mutation, and honest about the bounded in-flight-write risk without a Sentry CAS. Exact
   handoff and release remain the normal project-level promotion paths; no user boolean may
   steal ownership.
2. Turn the manual clean local demo sequence into a bounded Make target or script that
   creates no real credentials, records expected partial states, and is safe to rerun.
3. Add a source-tested local PagerDuty fixture helper, or remove the fixture-dependent demo
   from the default path. It must use a clearly invalid routing key and never claim delivery.
4. Add a capability-evidence workflow so the demo no longer requires a manual status patch.
   The controller must not infer capabilities from a version string alone.
5. Add an automated API report that lists created Sentry projects, keys, filters, PII
   settings, detectors, workflows, and legacy rules without exposing DSNs or tokens.
6. Improve Docker Desktop credential-helper diagnostics around local image builds while
   preserving a fail-closed default.
7. Continue non-external backlog work from the English task progress documents, with focused
   tests and evidence-boundary updates.

## Resume checklist

```sh
git status --short
git diff --check
make help
make verify-english
make verify-sentry-self-hosted
```

Then read `docs/development-runbook.md`, inspect the active Docker/kind state, and choose
either a clean rebuild or a read-only status check. Never assume that a previous local stack
still exists merely because a progress document records a successful past run.
