# Project status and continuation guide

Last reviewed: 2026-11-01.

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
- typed inbound filters, PII settings, behavior settings, and team-only issue ownership;
- readable Slack, PagerDuty, and Telegram notification targets with protected identity
  pinning and fail-closed re-resolution;
- cluster-scoped self-service `SentryAlertPolicy` resources, project opt-in by name,
  provider-first targets, protected per-child ownership, rollout status, and guarded
  deletion;
- legacy Sentry 24.7.1 issue/metric alert adapters and modern Sentry 26.10.1 metric
  detector/workflow adapters;
- protected Telegram registration, isolated hook-verification components, durable at-least-once
  queue, retry, dead-letter state, and isolated worker components; alert-source wiring is disabled;
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
| Sentry 24.7.1 feature-complete | Project/managed-client-key/settings, custom HTTP/S3/GCS symbol-source, issue-ownership, and historical isolated service-hook configuration; client-key and symbol-source credential rotation and guarded deletion; complete legacy issue and event-count/user-count metric alert CRUD; controller lost-response recovery, regional revision takeover and stale-writer fencing, DSN cutover, settings/symbol-source/ownership drift repair, and additional-team grant/revoke with primary-team preservation. The current project controller does not create a hook for alerts. The 2026-11-01 clean run also proved event ingestion, filtering behavior, operator-published DSN ingestion, and browser-visible issue creation. It did not prove notification delivery. |
| Sentry 26.10.1 errors-only | Project/managed-client-key/settings, custom HTTP/S3/GCS symbol-source, issue-ownership, and historical isolated service-hook configuration; client-key and symbol-source credential rotation and guarded deletion; modern metric detector/workflow lifecycle; controller lost-response recovery, regional revision takeover and stale-writer fencing, DSN cutover, settings/symbol-source/ownership drift repair, and additional-team grant/revoke with primary-team preservation. The current project controller does not create a hook for alerts. The 2026-11-01 clean run also proved event ingestion, filtering behavior, operator-published DSN ingestion, and browser-visible issue creation. It did not prove notification delivery. |

Sentry 26.10.1 returns 404 for the legacy `/rules/` and `/alert-rules/` collection routes.
Legacy compatibility must remain not ready on that server.

The 2026-11-01 first-release kind acceptance used browser automation on both Sentry versions.
It proved managed project creation and deletion, external-project observation and retention,
DSN publication, filters/PII settings, fake local PagerDuty catalog pinning, complete legacy
alerts on 24.7.1, modern detector/workflow updates and enablement on 26.10.1, browser-induced
settings drift repair, operator-published DSN ingestion, browser-visible issues, Kubernetes
Events, and authenticated TLS controller metrics. The fake PagerDuty service did not deliver
a notification. Browser inspection also exposed a Sentry 24.7.1 list-rendering defect where
an issue rule and metric rule with the same numeric identifier can occupy one row; direct
search and detail views still showed the independently correct regression rule.

## First-release decision

The apparent deployed-controller adoption blocker in the first QA pass was an invalid test
scenario: it used `project.use`, whose API contract is permanently read-only. The supported
takeover path is `project.create` with the existing slug, an explicitly Active owned
connection, and `projectManagement.unmanagedProjects: Adopt`. Clean kind runs against both
supported Sentry versions adopted manually created projects without replacing their remote
identities, reported `Managed/Manage/Active` and `Ready=True`, and removed the ownership marker
while preserving the same project after Retain. The retained projects were verified through
both the public API and browser UI. Existing-project adoption is therefore **go within this
explicit policy boundary**.

The legacy alert engine now isolates an unsupported `enabled: false` request to that alert
child. A clean Sentry 24.7.1 run kept the other three legacy children Ready with their remote
objects present while the disabled child reported `AlertAdapterUnavailable` and the aggregate
reported `AlertsPartiallyApplied`. Re-enabling remains required because the legacy API has no
accepted non-destructive toggle. Modern 26.10.1 alert enable/disable remains independently
supported and browser-verified.

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

## Completed in-scope work

### Ownership evolution implemented with unit and envtest evidence

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
and envtest evidence. A strictly larger Active
revision now performs an exact pre-read of the pinned marker, updates only the compact remote
evidence, waits for a bounded five-second fencing interval, verifies a read-after-write, and
then advances protected state. A retry recovers when the remote write completed before the
protected-state update. Equal-revision conflicts, lower local revisions, foreign markers,
missing evidence, and replaced identities fail closed.
Clean configuration-only controller acceptance against Sentry 24.7.1 and 26.10.1 exercised
the update endpoint, changed the active region at a larger revision, observed the real
five-second fencing delay and exact readback, and proved that stale and equal-revision
conflicting writers leave the remote evidence unchanged. This is not a physical multi-cluster
or network-partition failover test, and the bounded in-flight-write risk remains because
Sentry exposes no proven conditional-write primitive. Explicit `unmanagedProjects: Adopt`
now acquires the exact local claim, pins the
observed remote identity, journals and audits the marker update, verifies it by readback,
and recovers an applied update after a lost response. Admission requires an explicitly
Active owned connection. Clean live runs on both supported versions first observed an
unmarked project as an External/ReadOnly standby candidate without creating a claim, rejected
its Active promotion under the Standby policy, then adopted it through the public update
endpoint without creating a replacement. The runs pinned its exact identity, verified the
regional marker, and finally released the marker under Retain while preserving the external
project and removing the protected claim and binding. Standby candidates pin an exact
existing project without creating or changing the manager claim.
An unmarked project is observable only under explicit `unmanagedProjects: Standby` policy
and remains External/ReadOnly. A candidate for a locally proven managed project may publish
an authorized DSN and manage only its separately owned alert children; every child mutation
revalidates the active regional evidence and external identity. Clean live runs on both
supported versions created a separately owned user-count alert through a managed read-only
candidate, then proved Retain deletes that exact legacy rule or modern detector/workflow
before releasing the candidate binding while preserving the manager claim, external project
identity, and regional evidence. The same runs also observed a locally proven managed project
through a read-only candidate without changing the claim, rejected a plain Active request,
consumed an exact handoff, and promoted the candidate while preserving the same external
identity and regional evidence. The same state machine can promote after acquiring a
genuinely free claim; that variant remains unit-tested because its only additional operation
is protected-state claim acquisition.
An External/ReadOnly candidate can promote only through the explicit Active-region Adopt
workflow. Promotion preserves candidate-owned child state and performs no Sentry mutation
unless adoption must write the first regional marker. Clean Retain release now removes
operator-owned alerts, destinations, and additional-team grants before it journals removal
of the exact remote ownership marker, verifies the marker is absent by readback, and
deletes the protected claim and binding. Lost release responses recover from the durable
journal without repeating the mutation. Read-only Retain removes only its local binding.
The managed Retain release path now has unit, envtest, and live Sentry evidence; child cleanup
ordering remains covered by unit and envtest tests. Local cleanup now deletes the UID-scoped
binding before the global claim and recovers a same-project claim left after binding deletion.
It also removes a claim acquired before binding creation, preventing a transient create failure
from permanently blocking the slug.

The service-hook lifecycle remains an isolated, contract-tested component, but the project
controller no longer creates a hook from alert configuration. A project event hook cannot prove
that a first-seen, regression, event-count, or user-count alert fired. The earlier alpha wiring
would therefore have fanned every event out to every configured route. Reconciliation removes
any previously recorded hook, and the public alert selector admits only Slack and PagerDuty.
Telegram alert-source wiring remains deferred until an authenticated firing identity or a
durable trigger evaluator is available.
Live API and full-controller acceptance on Sentry 24.7.1 and 26.10.1 proved the
active-on-create behavior, unpublished staging publication, protected identity pinning,
revision-triggered replacement, convergent read-back, old-hook cleanup, and exact final
removal without notification delivery.

The reusable alert-policy design decision is complete: `SentryAlertPolicy` is cluster
scoped, ordinary cluster RBAC controls policy CRUD, and any project may opt in. There is no
policy owner field or custom authorization webhook. Policy and project targets use separate
ownership scopes, exact UIDs are persisted in protected bindings, updates enqueue all
consumers, and a finalizer holds deletion until policy-owned children are gone. Runtime and
generated-contract verification pass in unit and envtest suites. Clean live runs on both
supported Sentry versions exercised initial rollout and an in-place policy generation
update through `RolloutComplete=True`. This proves configuration reconciliation, not
external notification delivery.

The closed mutation audit now covers managed project lifecycle, alert mutations, Telegram
registration, destination cleanup, delivery attempts, and delivery-queue retention. Expired
admission candidates, sent tombstones, and dead letters are exact-deleted only after an
identity-free immutable audit start record is durable; success, already-absent recovery,
precondition conflict, and API failure receive closed-schema outcomes. An audit outage
fails closed before deletion. Source tests prove that route keys, dedupe keys, and message
payloads cannot enter these retention events. There is intentionally no queue replay or
force-delete administration command.

The protected-state backup core now has both envtest and live kind round-trip evidence. The
bounded kind drill keeps its sensitive bundle in memory, covers binding, claim, operation,
challenge, route, hook-source, admission, and InFlight/Retry/DeadLetter records, performs
exact deletion followed by create-only and resumable restore, verifies a stable digest, and
rejects tampering or live conflicts. It refuses namespace collisions and removes only its
four ownership-labeled namespaces. Persisted encryption, key management, CR/input-Secret
backup, retained audit restoration, and a full installation restart remain platform backup
responsibilities rather than a plaintext operator command.

### Project configuration expansion backlog

The project contract now covers five standard inbound filters, five PII fields, five
independently reconciled behavior fields, portable advanced PII/grouping rules, the twelve
built-in symbol sources shared by both supported versions, and separately owned Secret-backed
HTTP, S3, and GCS custom symbol-source children.
The filtered-transaction toggle and a validated set of eight non-deprecated legacy-browser
subfilters have matching source contracts on Sentry 24.7.1 and 26.10.1 plus local adapter and
engine tests. The five behavior fields also have matching source contracts on both versions,
bounded API validation, fresh read/conditional update behavior, and unit/envtest evidence.
Clean live HTTP and controller acceptance on both versions covers both expansions, including
drift repair followed by mutation-free convergence. Expand further through typed,
independently reconciled components; do not add an arbitrary Sentry settings map. Every new
mutation path must use the regional ownership fence, preserve unmanaged external objects,
and gain explicit capability evidence on both supported self-hosted versions before it is
described as portable.

The separate `issueOwnership` component now supports bounded ordered path/module/URL/tag
rules with readable team owners, explicit fallthrough and auto-assignment modes, and
CODEOWNERS synchronization for already imported data. Admission and policy require every
owner team to be the primary project team or a declared additional grant authorized by the
connection. Raw ownership grammar, direct user/email owners, repository IDs, code mappings,
and CODEOWNERS import are handled by a separate alias-only component. Clean configuration-only public API
and controller runs on both supported versions proved round-trip update, drift repair with
exactly one ownership PUT, and a following no-op reconcile.

Keep high-churn delivery artifacts outside controller ownership. Releases, deploys, source
map uploads, debug information file uploads, environments, events, issues, and feedback
belong to CI/CD or observed runtime state. Organization-wide billing, spend allocation, and
quota policy also require a separate organization-level contract. External IDs, hook
secrets, private DSNs, symbol-source credentials, security tokens, and raw secret-bearing
payloads must remain in Secret references or protected state, never public specs or status.

1. **Implemented and configuration-live-accepted on both supported versions.** The self-service ownership and
   regional failover contract does not change the
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
   steal ownership. Clean controller acceptance on 24.7.1 and 26.10.1 proved a live remote
   marker transition between regions at a larger revision, the real five-second fencing
   delay, exact readback, audit completion, and refusal of stale and equal-revision
   conflicting writers. Physical multi-cluster failover and network-partition behavior remain
   outside this configuration-only evidence boundary.
2. **Isolated component contract implemented and historically live-accepted on both
   supported versions; current alert wiring is disabled.** The service-hook lifecycle engine
   can create or recover exactly one owned project hook, pin its external identity, publish
   protected source/verifier state in fail-closed order, rotate HMAC secrets through an
   overlap, verify each mutation, and remove only the exact owned hook. Historical
   configuration-only API and controller acceptance on 24.7.1 and 26.10.1 proved active
   create behavior, staging publication, redaction, revision rotation, convergent read-back,
   old-hook cleanup, and exact deletion. The current project controller deliberately does not
   invoke that lifecycle from alerts and removes earlier hook state because a raw project
   event cannot preserve alert-trigger semantics. Public DNS, trusted certificates, an
   authenticated alert-firing source, and real delivery remain separate evidence boundaries.
3. **Implemented and live-accepted for the portable common fields.** The standard
   filtered-transaction and legacy-browser filters have typed contracts with validated
   subfilters. Auto-resolve age, default environment, crash-report retention, and
   JavaScript/source-map scraping use a separate typed behavior component. Allowed domains
   own a non-empty set of DNS hostnames or HTTP(S) origins, with an optional leading wildcard
   subdomain and fixed port; a sole `*` entry permits every origin. Clean public API and
   controller runs on both supported versions proved update, read-back, drift repair, and
   no-op convergence for all five behavior fields.
   Automatic release creation remains outside the common surface because 24.7.1 does not
   expose the 26.10.1 field. Keep TLS verification under platform policy. Treat custom
   inbound filters as an experimental, capability-gated child resource rather than folding
   their condition trees into the common settings contract.
4. **Implemented and live-accepted for team owners.** Project issue ownership uses a
   separate `issueOwnership` contract for bounded structured rules, fallthrough behavior,
   auto-assignment, and CODEOWNERS synchronization. Readable team references are authorized
   through project attachment and connection policy. Direct user/email owners, raw grammar,
   raw external repository identities remain outside the alpha contract. Repository linkage
   and CODEOWNERS import use the separate alias-only `codeMappings` component.
5. **Implemented and live-accepted for configuration lifecycle.** Managed client keys are
   separately owned children with explicit creation, activation, portable rate limits,
   monotonic rotation, and guarded deletion while only public DSNs are published. Durable
   create journals, ownership-bearing remote labels, exact protected identity pinning, and
   staged Secret cutover prevent interrupted creates, same-name replacements, and partial
   rotations from selecting unrelated keys or exposing secret material. Source, adapter,
   engine, webhook, and controller tests cover the lifecycle. Clean configuration-only runs
   on both supported Sentry versions proved create, active/rate update, rotation, public-DSN
   cutover, guarded deletion, and convergent cleanup through direct API and full-controller
   gates. Event ingestion and notification delivery were intentionally not exercised.
6. **Portable subset implemented and live-accepted; incompatible fields closed as a documented
   version boundary.** Typed grouping enhancements,
   fingerprinting rules, and advanced Relay PII rules now use bounded enums, exact selector
   paths, ordered lists, and canonical serialization; raw JSON, raw rule grammar, regex rules,
   and settings dictionaries remain unavailable. Both pinned Relay bindings accepted the exact
   generated PII JSON/selectors, both native enhancement bindings accepted the generated rule
   text, and source inspection confirms the fingerprinting grammar on both versions. Grouping
   profiles and secondary-grouping migration cannot join the common contract: 24.7.1 accepts
   their project fields, while 26.10.1 explicitly rejects manual updates to all three fields.
   Focused, CRD, controller, and clean configuration-only public API/full-controller runs on both
   versions proved update, read-back, drift repair, and no-op convergence for the portable subset.
   Event ingestion and notification delivery were intentionally not exercised.
7. **Symbol sources, repository aliases, code mappings, and Sentry CODEOWNERS storage are
   live-accepted; provider-backed CODEOWNERS fetch remains external.**
   The common typed settings contract owns the complete set of the twelve built-in sources
   present in both supported versions, with deterministic ordering, validation, drift repair,
   and explicit empty-set disablement. Clean public API and full-controller runs on both
   versions proved update, read-back, drift repair, and no-op convergence. The 26.10.1-only
   `proton` source and `enableNativeSourceServerMapping` field remain outside the common
   contract because 24.7.1 exposes neither. Custom HTTP, S3, and GCS sources use
   project-namespace Secret references, deterministic protected identities,
   ownership-bearing names, lost-create recovery, credential rotation, visible-drift repair,
   and exact cleanup. Clean public API and full-controller runs on both versions proved the
   complete configuration lifecycle. Azure remains excluded because it is not portable.
   Source-code integrations and repositories now use stable connection-owned aliases. The
   project `codeMappings` component owns exact path mappings and optional CODEOWNERS imports;
   numeric integration, repository, mapping, and CODEOWNERS identities stay in protected
   state. Durable journals recover lost creates without adopting an unjournaled exact tuple,
   and regional authorization is revalidated before every mutation. Source-contract, focused,
   admission, and controller envtest evidence passes. Clean configuration-only direct API
   runs on Sentry 24.7.1 and 26.10.1 proved integration/repository catalog resolution,
   mapping create/update/read-back/delete, and Sentry CODEOWNERS child create/read-back/delete.
   Full-controller runs on both versions proved alias resolution, mapping create, visible
   drift repair, mutation-free convergence, safe public status, protected identity storage,
   and exact cleanup. The disposable catalog intentionally points to an invalid external
   GitHub installation, so fetching a real repository CODEOWNERS file was not exercised and
   remains an external credential/provider boundary. Symbol download, native symbolication,
   event ingestion, and notification delivery were not exercised.
8. **Evaluated and closed as a version boundary.** Managed client keys already provide the
   portable count/window rate limit. Project spike protection has only a raw internal option
   in detailed serialization, without a bounded typed public update contract. Sentry 24.7.1
   accepts feature-gated project `dynamicSamplingBiases`; 26.10.1 only reads that legacy
   shape and moves manual target rates to a private organization-scoped bulk endpoint that
   requires numeric project IDs, a custom feature, and project sampling mode. No common
   alias-only mutation/disable contract exists, so project quota and dynamic-sampling
   controls intentionally remain unavailable in v1alpha1.
9. **Implemented and live-accepted on Sentry 26.10.1.** `kind-local-demo`,
   `kind-local-demo-status`, and
   `kind-local-demo-clean` pin every kubectl operation to an explicit `kind-*` context, load
   only the ignored mode-0600 local-stack token, apply the documented capability evidence,
   wait for the expected independently ready components, and report the known partial
   states. The workflow is rerunnable, creates no external credentials, and cleanup removes
   only five named project resources, two named connections, the exact token Secret, and
   four namespaces carrying the demo ownership label. A clean local run with the current
   operator image reconciled the modern and read-only projects to `Ready=True`, preserved
   the documented legacy, unresolved-destination, and paused partial states, and left no
   ownership-labeled demo namespace after cleanup. The workflow deliberately does not create
   kind, Sentry, cert-manager, or the operator deployment.
10. **Implemented and live-accepted on both supported versions.** Local Sentry bootstrap creates or repairs the exact
   `SKO Demo PagerDuty` integration and `Demo On-call` service only inside the disposable
   version-scoped organization. Its routing key is the literal
   `invalid-routing-key-not-for-delivery`; its synthetic numeric service ID satisfies the
   Sentry workflow form's integer contract without identifying an external PagerDuty service.
   Repository checks pin both values and the version-specific model imports. This fixture
   proves catalog resolution and Sentry alert configuration only and must never be described
   as PagerDuty delivery evidence.
11. **Implemented and live-accepted on both supported versions.** Successful local configuration/full API acceptance
   writes a mode-0600 evidence record containing the exact version, organization, gate, and
   two proven safe-create capabilities. The bounded demo validates the complete record before
   applying connection status; it never derives capabilities from a version string. Stack
   recreation and removal delete the record so stale evidence fails closed. This local
   workflow is not SaaS or external-server capability discovery.
12. **Implemented and live-accepted on both supported versions.** `sentry-api-report` reads
   only the disposable local API and emits fixed safe projections for projects, managed key
   metadata/rate limits, inbound filters, PII booleans and field counts, legacy issue/metric
   rules, or modern detectors/workflows. It omits DSNs, token material, routing keys, raw
   Relay configuration, sensitive/safe field names, and all external object identities. The
   live configuration gates verified non-empty project/key projections and the selected legacy
   or modern adapter before exact fixture cleanup.
13. **Implemented and locally accepted.** Local image build and push targets run a static
   credential preflight that validates Docker context/configuration and every configured
   helper executable without reading credentials. An explicit diagnostic target probes each
   helper with a bounded timeout while discarding its output. Missing, locked, failing, and
   hanging helpers fail closed; no target rewrites Docker configuration or substitutes an
   unauthenticated configuration. The fail-closed source suite, redacted Docker Desktop
   helper probe, and a current local operator image build all passed. Registry push and
   hosted release evidence remain owner-deferred external work.
14. **Implemented and configuration-live-accepted on both supported versions.** The typed
   `user-count` trigger maps to `count_unique(user)` in both the Sentry 24.7.1 legacy metric
   rule adapter and the Sentry 26.10.1 metric detector adapter. It shares the bounded
   window/threshold contract, ownership lifecycle, protected identity, and destination
   policy of `event-count`. Source, generated CRD, fixture, focused adapter coverage, and
   fresh-stack direct API plus full-controller acceptance pass on both versions. Sentry
   26.10.1 canonicalizes the requested aggregate to `count_unique(tags[sentry:user])` on
   read-back. This is configuration-only evidence and must not be described as notification
   delivery.
15. **Implemented and locally accepted across distinct code versions.** The default kind e2e
   suite now builds its baseline image from exact commit
   `ba5143fa704a923e251a31c71d8f32342ba81344` and its upgrade image from the current
   worktree. An isolated fake Sentry boundary commits one project-create POST, loses the
   response, and keeps the project hidden while the baseline controller persists
   `CreateProject`/`Submitted`. After the rollout, the current controller recovers the exact
   ownership marker, clears pending state, pins the identity, reaches `ProjectReady=True`,
   and completes Retain cleanup without a second POST. The same 8/8 run preserved unrelated
   protected pending/routing/delivery records byte-for-byte and deleted only its exact
   temporary cluster. This closes the local distinct-version pending-operation backlog item;
   it does not claim target-Sentry, hosted release, or rollback evidence.
16. **No owner-approved non-external backlog remains as of this review.** The remaining
   evidence boundaries in T06 through T10 are the explicitly deferred provider credentials,
   public ingress, production soak, distribution-specific Kubernetes, persisted encrypted
   full-installation backup, hosted upgrade/rollback, and hosted release work listed above.
   Do not reinterpret fixture, fake-server, source, envtest, or local kind evidence as proof
   of those external paths. Add any newly discovered locally actionable defect or contract
   gap here before implementation so the backlog remains explicit.

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
