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
- declarative additional project-team access with connection-policy authorization,
  protected team identity pinning, and unmanaged-team preservation;
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
| Sentry 24.7.1 feature-complete | Project/key/settings CRUD, legacy issue and metric alert CRUD, controller lost-response recovery, settings drift repair, and additional-team grant/revoke with primary-team preservation. This is configuration-only evidence; the earlier ingestion attempt was blocked by Relay memory health. |
| Sentry 26.10.1 errors-only | Project/key/settings CRUD, modern metric detector/workflow lifecycle, controller lost-response recovery, settings drift repair, and additional-team grant/revoke with primary-team preservation. This is configuration-only evidence. |

Sentry 26.10.1 returns 404 for the legacy `/rules/` and `/alert-rules/` collection routes.
Legacy compatibility must remain not ready on that server.

The 2026-10-31 clean kind acceptance used browser automation on both Sentry versions to
create teams and pre-existing projects before the operator attached read-only consumers.
It also proved managed project creation and deletion, existing-project retention, DSN
publication, filters/PII settings, additional-team access, fake local PagerDuty catalog
pinning, direct and policy-owned alerts, policy updates, and paused-project behavior. The
26.10.1 run repaired browser-induced settings drift within 9 seconds and exposed bounded
controller, alert-policy, condition-transition, and synchronization metrics. The 24.7.1
run accepted a decimal `20.0` metric threshold returned by Sentry. All of this remains
configuration-only evidence; the fake PagerDuty service did not deliver a notification.

Additional project-team access is covered by generated schema, admission, policy, domain,
HTTP adapter, and controller tests. On 2026-10-31 clean disposable-stack runs verified
grant, read-back, revoke, and primary-team preservation against both supported Sentry
versions. Direct user access remains outside the alpha contract.

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
Admission now prevents a configured ownership fence from being removed, rejects decreasing
revisions, and requires a larger revision for region changes or Standby-to-Active promotion.
The connection controller no longer echoes a requested Active revision as observed: it
reports `RevisionUnverified` and keeps the aggregate effective role Standby because a
connection has no single remote marker. For a newly managed project, the project engine now
encodes compact region, revision, owner, and installation evidence in the remote project
name, pins the complete marker in protected state, and marks that project Active only after
an exact readback. Standby connections are rejected before project mutation.

All managed child authorizers re-read that remote project evidence immediately before a
Sentry mutation and fail closed on a changed revision, fingerprint, or external identity;
guarded project deletion performs the same check in the project engine. This has unit-test
evidence only and must not be described as live failover support. The controller does not
yet take over an existing lower-revision marker with the required write-delay-read sequence,
promote standby project candidates, or perform adoption. Revision takeover is therefore the
first remaining ownership task.

The reusable alert-policy design decision is complete: `SentryAlertPolicy` is cluster
scoped, ordinary cluster RBAC controls policy CRUD, and any project may opt in. There is no
policy owner field or custom authorization webhook. Policy and project targets use separate
ownership scopes, exact UIDs are persisted in protected bindings, updates enqueue all
consumers, and a finalizer holds deletion until policy-owned children are gone. Runtime and
generated-contract verification pass in unit and envtest suites. Clean live runs on both
supported Sentry versions exercised initial rollout and an in-place policy generation
update through `RolloutComplete=True`. This proves configuration reconciliation, not
external notification delivery.

### Project configuration expansion backlog

The current project settings contract intentionally covers only three standard inbound
filters and five PII fields. Expand it through typed, independently reconciled components;
do not add an arbitrary Sentry settings map. Every new mutation path must use the regional
ownership fence, preserve unmanaged external objects, and gain explicit capability evidence
on both supported self-hosted versions before it is described as portable.

Keep high-churn delivery artifacts outside controller ownership. Releases, deploys, source
map uploads, debug information file uploads, environments, events, issues, and feedback
belong to CI/CD or observed runtime state. Organization-wide billing, spend allocation, and
quota policy also require a separate organization-level contract. External IDs, hook
secrets, private DSNs, symbol-source credentials, security tokens, and raw secret-bearing
payloads must remain in Secret references or protected state, never public specs or status.

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
2. Complete the controller-owned Sentry service-hook lifecycle needed by protected Telegram
   delivery. Create or recover exactly one owned project hook, pin its external identity,
   publish the protected source plan and verifier configuration in fail-closed order, rotate
   HMAC secrets through an overlap, verify the live hook after every mutation, and remove
   only the exact owned hook. Public DNS, trusted certificates, and real delivery remain
   separate evidence boundaries.
3. Expand stable typed project settings. Add the standard filtered-transaction and
   legacy-browser filters with validated subfilters, then evaluate auto-resolve age, default
   environment, crash-report retention, JavaScript/source-map scraping, allowed domains, and
   automatic release creation. Keep TLS verification under platform policy. Treat custom
   inbound filters as an experimental, capability-gated child resource rather than folding
   their condition trees into the common settings contract.
4. Add project issue-ownership configuration without colliding with regional operator
   ownership terminology. Use an `issueOwnership` contract for bounded ownership rules,
   fallthrough behavior, auto-assignment, and CODEOWNERS synchronization. Start with readable
   team references; direct user/email ownership stays outside the alpha contract until its
   authorization and identity-pinning model is designed.
5. Add managed client-key lifecycle as a separately owned child resource. Support explicit
   creation, activation, rate limits, rotation, and guarded deletion while continuing to
   publish only public DSNs. Use journals and protected identity pinning so interrupted
   creates, same-name replacements, or partial rotations cannot leak secret material or
   select an unrelated key.
6. Design typed grouping and advanced data-scrubbing contracts. Cover grouping profiles,
   grouping enhancements, fingerprinting rules, secondary-grouping migration, and advanced
   Relay PII rules only after canonical serialization and cross-version semantics are
   proven. Do not expose raw vendor JSON or an unvalidated settings dictionary.
7. Add native symbolication and repository configuration only through explicit child
   ownership. Built-in symbol sources and native source-server mapping may be typed project
   settings; custom HTTP/cloud symbol sources require Secret references and exact cleanup.
   Repository links, code mappings, and CODEOWNERS need stable connection-owned aliases so
   public resources never contain numeric Sentry or integration IDs.
8. Evaluate project quota protection after the core settings work: spike protection,
   client-key error rate limits, and manual dynamic-sampling targets/biases are useful but
   feature- and deployment-dependent. Keep them unavailable unless capability evidence
   proves safe read, update, drift repair, and disable behavior on the selected target.
9. Turn the manual clean local demo sequence into a bounded Make target or script that
   creates no real credentials, records expected partial states, and is safe to rerun.
10. Add a source-tested local PagerDuty fixture helper, or remove the fixture-dependent demo
   from the default path. It must use a clearly invalid routing key and never claim delivery.
11. Add a capability-evidence workflow so the demo no longer requires a manual status patch.
   The controller must not infer capabilities from a version string alone.
12. Add an automated API report that lists created Sentry projects, keys, filters, PII
   settings, detectors, workflows, and legacy rules without exposing DSNs or tokens.
13. Improve Docker Desktop credential-helper diagnostics around local image builds while
   preserving a fail-closed default.
14. Continue non-external backlog work from the English task progress documents, with focused
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
