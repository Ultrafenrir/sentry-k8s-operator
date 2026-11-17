# Development plan

The current milestone is centered on making `SentryConnection`, `SentryProject`, and
`SentryAlertPolicy` complete, reliable, and fully accepted against every claimed compatibility
profile. This is a sequencing priority, not a permanent limit on adding new CRDs. New fields and
resources must still be backed by public API contracts and live acceptance evidence. Sentry UI
availability alone is not sufficient reason to expose a Kubernetes API.

## Documentation deliverable

The milestone is not complete when the generated CRDs merely accept the fields. A person must be
able to configure every supported setting without reading Go types or controller code.

- keep `docs/configuration.md` as the human-readable reference for all three CRDs;
- document omitted versus explicitly empty ownership semantics;
- include complete, safe YAML examples for connection, reusable policy, and project-local settings;
- list bounded values for alerts, filters, grouping, PII, client keys, destinations, lifecycle,
  ownership, and symbol sources;
- maintain an explicit Sentry 24.7.1 versus 26.10.1 compatibility table;
- explain top-level and component conditions, protected-identity failures, and recovery states;
- keep `docs/quickstart.md` on the typed alert schema and link it to the full reference;
- update documentation in the same change whenever a public CRD field or compatibility rule moves.

Documentation examples must never contain real tokens, DSNs, passwords, raw Secret data, or
protected external identities. The reference must pass `make verify-english` and remain consistent
with generated CRD validation.

## Design direction

`SentryAlertPolicy` is a reusable policy layer, not a one-to-one representation of one Sentry
alert. It keeps common notification and detection rules out of every `SentryProject` and lets
application teams opt projects into centrally maintained defaults.

The policy model should eventually support composition without making project manifests verbose:

- a project may reference an ordered list of policies rather than only one policy;
- later policies may extend earlier policies only through explicit, deterministic merge rules;
- local project alerts may add rules or override an allowed subset by stable rule key;
- conflicting definitions must fail closed and identify the policy and key in status;
- rollout status must remain bounded and must not expose consuming project names;
- policy deletion must remain protected while a project references it;
- resolving policies must produce one immutable effective alert specification before any Sentry API
  mutation is authorized.

Policy composition is future work. The current single `alertPolicy` reference remains the alpha
contract until one policy has stable effective-spec semantics, ownership rules, rollout behavior,
migration coverage, and restart-safe lifecycle tests.

## Validated current coverage

The current `SentryProject` surface already covers the most important project ownership features:

- create or observe a project, select its platform and primary team, and grant additional team
  access;
- publish a selected DSN and manage bounded server-generated client keys and rate limits;
- retain or guardedly delete a managed project;
- manage auto-resolution, default environment, crash-report storage, allowed domains, and
  JavaScript source fetching;
- manage the portable inbound-filter subset;
- manage basic and advanced PII scrubbing;
- manage grouping enhancements and fingerprint rules;
- manage built-in and custom symbol sources;
- manage team-based issue ownership, code mappings, and CODEOWNERS integration.

The Sentry 24.7.1 UI acceptance saved and reopened additional team access, PII fields, one advanced
PII rule, fingerprint and stack-trace rules, an ownership rule, and one HTTP symbol repository. The
Sentry Audit Log recorded every mutation. Code mappings remain covered by the public API contract
and operator tests even though the inspected project settings menu did not expose a dedicated code
mapping page.

The Sentry 26.10.1 UI acceptance created a second project and saved or reopened all representative
settings used by the three core CRDs. The project has two teams, custom origins and source header,
modern inbound filters, dataset-specific PII rules, ownership rules, grouping rules, one HTTP symbol
repository, and enabled JavaScript loader features. The modern UI adds release auto-creation, SCM
source context, OTLP endpoints, logs and metrics, user feedback, log-message filters,
application-metric filters, and separate PII datasets for errors/transactions/attachments, logs,
and application metrics. SCM source context and source TLS verification did not persist without the
required integration or backend support and therefore must not be treated as portable fields.

The same stand created and reopened these remote objects through the UI:

- one automatically created Error Monitor;
- error-count threshold, percent-change, and dynamic metric detectors;
- log-count, span-duration, and crash-free-session metric detectors;
- one Cron Monitor with schedule, margins, failure and recovery tolerances, owner, and description;
- one project-scoped team notification alert and one PagerDuty alert with explicit critical
  severity.

The Audit Log confirms that Sentry 26 persists metric and Cron detection as `detector` objects,
notification behavior as reusable `workflow` objects, and their attachment as separate
`detector_workflow` links. This separation is an important API boundary for the operator and not
only a UI detail.

`SentryConnection` already has the correct narrow responsibility: endpoint, credentials,
transport, organization selection, compatibility profile, regional fencing, unmanaged-project
policy, and readable catalogs for installed Slack, PagerDuty, and source-code integrations.

The largest functional gap is `SentryAlertPolicy`. Its current `trigger`, `windowMinutes`,
`threshold`, `enabled`, and `notify` fields cover only a small subset of the alert editors
available in either supported Sentry version.

## Sentry object classification and future CRDs

The Sentry 26.10.1 UI groups several concepts under Monitors and Alerts, but they are not the same
kind of remote object and must not share one overloaded Kubernetes schema.

| Sentry concept | Relation to alerts | Current plan |
| --- | --- | --- |
| Issue Alert | A workflow attached to the project Issue Stream detector | Model reusable conditions and actions in `SentryAlertPolicy`; keep the detector/workflow link explicit internally. |
| Metric Monitor or Metric Alert | A detector evaluated from a metric time series and linked to zero or more workflows | Model the detector as a typed policy rule and reuse policy workflows rather than duplicating actions. |
| Threshold, Change, and Dynamic detection | Detection algorithms of a metric alert | No new CRD. Add typed, compatibility-gated fields to the metric rule. |
| Logs and Application Metrics | Metric datasets and presets | No new CRD. Add version-gated metric presets only after API and UI acceptance. |
| Cron Monitor | A check-in monitor plus detector, schedule, missed-check rules, runtime limits, and workflow links | Add later as a separate namespaced `SentryCronMonitor` after the core three CRDs are complete. |
| Uptime Monitor | An endpoint-check detector with request configuration, assertions, thresholds, and workflow links | Add later as a separate namespaced `SentryUptimeMonitor` after the core three CRDs are complete. |
| Seer workflow or action | An analysis or automation step attached to alerts and issues | No new CRD now. Treat as a future typed action only after a stable public API exists. |
| Email, PagerDuty, Slack, GitHub, and other actions | Alert delivery or side effects | Model as typed policy actions, not independent CRDs. |
| Sentry-created default Error Monitor | A default alert created during project onboarding | Handle through an explicit project or policy lifecycle decision, not a new CRD. |

No monitor CRD belongs in the current implementation milestone because the existing three CRDs must
be completed first. This does not block future resources. Adding Cron or Uptime creation is a normal
product-surface expansion once the connection, project, and alert-policy lifecycles are demonstrably
stable; it still requires a focused API and lifecycle proposal rather than being folded into an
unrelated resource.

If that expansion is approved, independently owned monitor instances should be namespaced resources
because application teams will create many of them, update them independently, and need bounded
per-object status and finalization. Do not place a large monitor map inside `SentryProject`.

The future proposal should compare:

- separate `SentryCronMonitor` and `SentryUptimeMonitor` CRDs, which give clear schemas and
  lifecycle behavior; and
- one `SentryMonitor` CRD with a strict `kind` discriminator, which reduces CRD count but creates
  a larger union schema.

Separate CRDs are the safer default because schedules, check-ins, URLs, assertions, credentials, and
failure semantics differ substantially. Both resources may reference `SentryProject` and may
reuse a bounded notification component from `SentryAlertPolicy`, but the alert policy must not own
their remote lifecycle.

## Alert-policy architecture for Sentry 26

Keep `SentryAlertPolicy` as the reusable Kubernetes abstraction even though modern Sentry exposes
separate detector, workflow, and link APIs. Internally resolve a policy into two immutable parts:

1. detector templates describing issue or metric detection; and
2. reusable workflows describing filters, actions, recipients, severities, and throttle.

For each consuming project or future monitor, reconciliation must create or observe the detector,
create or reuse the workflow, and reconcile the explicit link. A workflow must not be duplicated
merely because several detectors use the same policy. Protected state must hold ownership and
desired hashes for all three remote identities without publishing them in CR status. Delete must
unlink first, remove an operator-owned detector only after ownership verification, and remove an
operator-owned workflow only after no managed links remain.

The alpha CRD may keep a compact `rules` shape, but adapters must not flatten the modern object
graph into one opaque payload. Add pure planning tests that prove stable detector/workflow/link
plans, deterministic ordering, reuse, safe unlinking, and recovery after interruption between any
two mutations. The legacy adapter may translate the same effective representation to its older
single-rule API.

## Immediate alert work

1. Make PagerDuty severity explicit and bounded. The 26.10.1 UI now defaults cleanly and the live
   critical-severity workflow reopened correctly, so the remaining work is deterministic adapter
   mapping and old/new contract coverage rather than a UI-specific `undefined` workaround.
2. Introduce one typed effective alert representation with explicit issue and metric variants.
   Policy and project-local rules must resolve into this representation before adapter selection.
3. Validate every alert capability against the selected connection compatibility profile before
   external I/O. Unsupported combinations must report a precise validation reason instead of
   reaching `AlertAdapterUnavailable` during project reconciliation.
4. Decide how operator-created projects handle Sentry's automatically created Error Monitor.
   Expose `Disabled`, `Unmanaged`, or `Managed` behavior only if public APIs provide a safe,
   repeatable lifecycle contract.
5. Improve managed object display names. Keep complete desired hashes and ownership identities in
   protected state; shortening a UI label must not weaken crash recovery or ownership verification.
6. Represent modern detector/workflow/link ownership explicitly and verify that one workflow can be
   reused by several managed detectors without duplication or unsafe deletion.

## Metric alert expansion

The typed metric rule should eventually contain:

- a bounded metric preset;
- optional environment selection;
- a bounded evaluation interval;
- static or compatible change detection;
- threshold direction (`above` or `below`);
- critical/high, optional warning/medium, and explicit resolution thresholds;
- a version-gated comparison period for change detection;
- typed actions selected by alert status;
- a team owner and human-readable description;
- notification severity or priority;
- explicit enabled or muted behavior.

The portable initial preset set should be the verified intersection of the supported versions:

- error count;
- affected users;
- crash-free sessions;
- crash-free users;
- throughput;
- duration;
- failure rate;
- largest contentful paint.

Sentry 24.7.1 also exposed Apdex, first input delay, cumulative layout shift, and custom
measurements. Sentry 26.10.1 additionally exposed logs, application metrics, and newer span
datasets. These must be compatibility-gated extensions, never silent translations to another
metric.

The Sentry 24.7.1 UI accepted intervals of 1, 5, 10, 15, or 30 minutes and 1, 2, 4, or 24 hours. A
saved live rule verified critical, warning, and resolved thresholds, PagerDuty delivery, and a
5-minute static interval. The same rule was then changed and saved as a percent-change rule using a
one-hour interval and the same time one week earlier as its comparison period.

The modern adapter currently fixes the dataset to errors, uses an empty query, selects all
environments, creates one high `greater than` condition, resolves at zero, and notifies on every
trigger. Each new field must replace one of those hard-coded choices and must be observed back from
Sentry before the condition becomes Ready.

The 26.10.1 creation audit provides concrete modern payload targets:

- threshold detection supports `above` or `below`, high and optional medium thresholds, and default
  or custom resolution;
- change detection adds a percentage, direction, and comparison window;
- dynamic detection adds responsiveness (`high`, `medium`, or `low`) and anomaly direction
  (`above`, `below`, or both);
- the verified datasets and aggregates are Errors `count()`, Logs `count(message)`, Spans
  `p95(span.duration)`, and Releases `crash_free_rate(session)`;
- Application Metrics supports multiple metric rows and equations, but the UI requires an ingested
  metric before a monitor can be saved;
- every detector supports project, optional environment, interval, typed query/filter, owner,
  description, and zero or more linked alert workflows.

The dynamic detector was created even though the UI returned an internal-error toast. It then
appeared in both the monitor list and Audit Log. Acceptance automation must therefore verify the
remote list and audit trail after ambiguous UI or API failures instead of blindly retrying and
creating duplicates.

Arbitrary Sentry query text, custom measurement expressions, and unbounded aggregation syntax
remain deferred until their validation, portability, and round-trip behavior are defined. A bounded
query subset may be proposed separately after environment and typed presets are stable.

## Issue alert expansion

Issue alerts must be modeled separately from metric detection. The target typed model should
support:

- multiple triggers in one rule;
- explicit `all` or `any` trigger matching;
- multiple typed filters with explicit `all` or `any` matching;
- optional environment selection;
- multiple typed actions;
- a bounded action interval or throttle;
- a team owner and human-readable description;
- explicit enabled or muted behavior.

The Sentry 24.7.1 UI verified these trigger families:

- new issue;
- resolved to unresolved regression;
- archived to escalating transition;
- event count;
- affected-user count;
- affected-session percentage.

The verified legacy filter families are issue age, approximate frequency, assignment, latest
release, category, event attribute, event tag, and event level. The modern editor additionally
exposes priority, issue type, de-escalation, release age, and related state filters. Add a small
typed common subset first, then compatibility-gated filters. Do not expose a free-form command or
condition list.

The Sentry 24.7.1 UI saved and reopened an issue alert with two triggers, a fatal-level filter,
PagerDuty delivery, a 30-minute action interval, and a team owner. Legacy throttle options were 5,
10, 30, or 60 minutes; 3, 12, or 24 hours; 1 week; and 30 days. The modern editor exposes the same
bounded family plus immediate delivery.

Slack and PagerDuty remain the first portable actions. Email is the next reasonable typed action.
Assignee or team notification may follow once recipient ownership is defined. GitHub or other
ticket creation, legacy integration fan-out, Seer actions, and arbitrary installed integrations
require separate contracts and remain deferred.

The 26.10.1 UI saved two reusable workflows and confirmed these modern source scopes:

- all issues in selected projects;
- selected detectors or monitors; and
- all issues in all projects.

The modern workflow builder supports multiple trigger rows, `all` or `any` filter matching,
if/then blocks, immediate or bounded throttling, team/member/suggested-assignee notification,
PagerDuty severity (`default`, `critical`, `warning`, `error`, or `info`), and GitHub ticket
creation. The first implementation must keep source scope bounded to selected managed projects or
managed detectors. Organization-wide workflow attachment is too broad for a namespaced policy and
must be rejected.

## Project improvements

### Priority 1: portable project fields

Add the smallest fields that are independently useful and verified in both API behavior and UI:

1. an optional human-facing project display name that does not replace the stable Kubernetes or
   Sentry slug and does not weaken ownership markers;
2. boolean inbound filters for hydration errors and chunk-load errors;
3. bounded custom inbound filters for IP or CIDR values, release patterns, and error-message
   patterns;
4. compatibility-gated log-message and application-metric filters only where the target Sentry
   version exposes them.

The Sentry 24.7.1 UI saved browser-extension filtering, an IP range, and a release pattern. It also
exposed hydration and chunk-load filters that are absent from the current CRD.

### Priority 2: project notification defaults

Add these only together with a supported email alert action:

- email subject prefix;
- email subject template;
- minimum digest interval;
- maximum digest interval.

These are project-wide notification defaults, not properties of project creation. The old UI saved
a custom subject prefix and exposed the subject template and digest controls. The creation wizard's
initial alert choices must continue to be expressed through `SentryAlertPolicy`, not copied into
`SentryProject`.

### Priority 3: optional source-fetching and loader settings

Evaluate these only after the higher-priority portable fields:

- a source-fetching security token read through a fixed namespace-local Secret reference;
- the corresponding bounded header name;
- TLS verification for source fetching;
- managed client-key loader options for performance monitoring, session replay, and debug bundles.

The modern UI additionally exposes JavaScript loader switches for logs and metrics, user feedback,
and SDK debugging, and displays OTLP log and trace endpoints. Treat loader switches as bounded key
configuration, not as project creation fields. OTLP endpoints are derived outputs and must never be
copied into desired spec. If exposed at all, publish only non-secret derived endpoints and continue
to keep key material in Secrets.

These settings are security-sensitive or specific to Sentry's JavaScript loader. They must not
place token values in CRDs, status, logs, or protected-state diagnostics, and they should not be
added merely because the UI exposes them.

### Project settings that remain outside the CRD

Do not add these to `SentryProject` without a separate product proposal:

- uploaded source maps, ProGuard mappings, and debug files, which belong to build and release
  pipelines;
- release and deploy tokens or release webhooks;
- security-report endpoints derived from a DSN;
- project transfer;
- environment creation, because environments are derived from ingested events;
- legacy plugins and project data-forwarding integrations;
- arbitrary source-code, grouping, PII, or filter grammar when a bounded typed representation
  exists.

## Connection and organization boundary

The Sentry 24.7.1 global settings UI exposed organization name, membership roles, member
permissions, attachment and debug-file access, teams, members, SSO providers, audit log, Relay
keys, repositories, installed integrations, early features, auth tokens, and custom integrations.
These settings do not all belong in `SentryConnection`.

Keep the following outside the current operator product surface:

- organization member and team lifecycle;
- membership roles and user permissions;
- 2FA and SSO configuration;
- avatar and organization deletion;
- Relay registration keys;
- early-adopter feature flags;
- API auth-token and custom-integration lifecycle;
- installation of external integrations.

Kubernetes RBAC on namespaced `SentryProject` remains the authorization boundary for selecting an
existing primary team and `grantAccess` teams. `SentryConnection` catalogs installed
integrations and repositories but does not install or configure their external credentials.

One future decision remains open: whether platform administrators need an optional, narrowly typed
organization privacy baseline. A separate `organizationPolicy` block on `SentryConnection`
could potentially cover required data scrubbing, required default scrubbers, global IP removal,
global sensitive and safe fields, and enhanced privacy. This must not be implemented until
ownership, project override behavior, rollback, and compatibility are explicitly designed. The
default plan is to leave organization-wide mutation unmanaged.

## Future resource blueprints

These resources follow the three-core-CRD milestone. They are not required to declare the current
milestone complete, but the design below is sufficiently concrete for a later implementation agent
to start with API discovery and contract tests rather than repeating the UI audit.

### `SentryCronMonitor`

Use a namespaced resource with a required `projectRef` and an optional `alertPolicyRef`. Its typed
spec should contain:

- stable display name or slug intent;
- schedule union: crontab plus IANA timezone, or a bounded interval schedule;
- check-in grace period and maximum runtime;
- consecutive failure and recovery tolerances;
- optional environment, team owner, and description;
- enabled state and guarded deletion policy.

The 26.10.1 UI saved `*/5 * * * *`, UTC, a five-minute grace period, ten-minute maximum runtime,
three failures, two recoveries, a team owner, and a description. The create UI also supports SDK
upsert and manual creation. The CRD should manage configuration only; it must not attempt to emit
application check-ins. Status should be bounded to conditions, observed generation, compatibility,
and a safe external URL. Do not mirror check-in history or remote IDs into status.

Implementation order for this resource:

1. confirm public create/read/update/disable/delete and check-in APIs for every claimed profile;
2. add source types, generated CRD/RBAC, adapter interfaces, protected-state ownership, and a
   finalizer;
3. reconcile the monitor/detector before linking the referenced alert-policy workflow;
4. add schedule validation, timezone validation, duration bounds, and cross-field tolerance tests;
5. cover create, update, disable, policy relink, delete, interrupted mutation, and operator restart;
6. accept through UI, Kubernetes status, operator logs, and a real check-in transition.

### `SentryUptimeMonitor`

Use a separate namespaced resource with required `projectRef` and optional `alertPolicyRef`. Its
typed spec should contain:

- URL and method;
- bounded interval and timeout;
- request headers whose values come only from namespace-local Secret references;
- optional request body from a Secret reference if the public API supports it;
- sampling flag;
- consecutive failure and recovery tolerances;
- an ordered assertion tree supporting status code, JSON path, response header, and bounded
  AND/OR groups;
- optional environment, team owner, description, and enabled state.

The modern UI exposes GET and other method selection, custom headers, a 1-second to 1-minute timeout
control, status assertions, JSON-path assertions, header assertions, logical groups, and a required
preflight test. Creation could not complete on the reduced local profile because both an internal
health URL and a public test URL failed the Uptime preflight. This is stand evidence, not proof that
the API is unsupported. A future acceptance profile must run the Uptime checker services before
claiming support.

Uptime is a security-sensitive controller. Before external I/O, reject unsupported schemes,
userinfo, fragments, invalid ports, loopback, link-local, multicast, and private destinations
unless an explicit cluster-admin policy permits them. Re-resolve DNS at execution time, constrain
redirects, prevent DNS rebinding, redact header and body values, and never include them in status or
logs. The controller must not become an SSRF or secret-exfiltration path.

### Shared monitor behavior

Both resources may share internal project resolution, workflow-link planning, protected-state
storage, conditions, finalization, and read-after-write verification. They must not share one public
union CRD: their validation, credentials, remote lifecycle, and operational failure modes differ.
An internal `MonitorNotificationRef` may reference `SentryAlertPolicy`, but notification actions
must stay owned by the policy workflow rather than copied into each monitor spec.

Dynamic anomaly detection is now proven to create on the 26.10.1 stand, but remains a
compatibility-gated `SentryAlertPolicy` capability rather than a new CRD. Seer workflows, ticket
actions, raw third-party payloads, arbitrary query languages, and organization administration
remain deferred capabilities.

## Compatibility comparison

The forms below were inspected in live local installations of Sentry 24.7.1 and 26.10.1. The
legacy follow-up acceptance also saved representative project settings and alert rules and verified
them through the reopened UI and Sentry Audit Log.

| Area | Sentry 24.7.1 | Sentry 26.10.1 | Operator implication |
| --- | --- | --- | --- |
| Project creation | Platform, one of three initial alert choices, project name, and team | Platform, initial alert frequency, email or messaging choice, slug, and team | Keep creation small. Express reusable alert behavior through `SentryAlertPolicy`; do not mirror either wizard in `SentryProject`. |
| Alert entry points | Issue, Number of Errors, and Users Experiencing Errors | Metric, Cron, Uptime, and a separate Issue Alert editor | Keep issue and metric rules in the policy. Cron and Uptime need independent lifecycle resources if added later. |
| Metric presets | Errors, affected users, crash-free sessions/users, throughput, transaction duration, Apdex, failure rate, LCP, FID, CLS, and custom measurement | Errors, affected users, span throughput/duration/failure rate/LCP, logs, application metrics, and release crash-free rates | Start with the verified intersection and gate additional presets by compatibility profile. Never silently translate a missing preset. |
| Metric window | 1, 5, 10, 15, or 30 minutes; 1, 2, 4, or 24 hours | Dataset-dependent interval selection | Model a bounded duration and validate it against the metric and compatibility profile. |
| Metric filtering | Project, environment, event type, and query | Project, environment, dataset, aggregate, interval, and query | Add environment first. Keep arbitrary query and aggregation syntax deferred. |
| Detection | Static or percent change; critical, optional warning, and resolved thresholds; above/below direction | Saved threshold, change, and dynamic detectors; dynamic uses responsiveness and anomaly direction | Implement portable static thresholds first, then compatible change detection. Add dynamic only to the modern compatibility profile with ambiguous-result recovery. |
| Metric actions | Per critical/warning state; email and installed PagerDuty in the inspected stand | Detectors link to reusable workflows; saved PagerDuty critical severity | Add explicit action status and severity mapping and model detector/workflow links rather than embedding modern actions in detector payloads. |
| Issue triggers | New issue, regression, escalation, event count, affected users, and affected sessions | New, resolved, escalated, regressed, activity captured, and Seer-stage choices | Add verified state transitions individually. Do not expose Seer-specific states in the portable core. |
| Issue filters | Age, frequency, assignment, latest release, category, event attribute, tag, and level | Adds priority, type, de-escalation, release age, and related modern filters | Introduce a typed common subset, followed by compatibility-gated filters. |
| Issue actions | Assignee/team/member notification, legacy integrations, PagerDuty, and GitHub issue creation | Preferred channel, PagerDuty, GitHub, and additional installed integrations | Keep Slack and PagerDuty first, add email next, and defer ticket creation and generic integration dispatch. |
| Issue throttle | 5, 10, 30, or 60 minutes; 3, 12, or 24 hours; 1 week; 30 days | The inspected editor exposes the same bounded family plus immediate delivery | Share one bounded throttle type with explicit compatibility validation. |
| Project filters | Core filters plus hydration, chunk-load, IP, release, and message filters | Adds newer log and application-metric filters | Expand the typed common subset first; version-gate newer filters. |
| Project privacy | Project PII, advanced PII, crash-report storage, and source-fetching controls | Saved separate advanced rules for errors/transactions/attachments, logs, and application metrics | Preserve the bounded typed model and Secret references; add a dataset discriminator and never expose raw Relay JSON. |
| Client keys | DSN and loader-oriented key configuration | Adds OTLP logs/traces, logs and metrics, user feedback, SDK debugging, and key use case | Keep credentials in Secrets. Treat OTLP URLs as derived output and loader switches as optional bounded configuration. |
| Modern object graph | Legacy alert APIs present one alert-oriented lifecycle | Audit Log records `detector`, `workflow`, and `detector_workflow` separately | Keep one user-facing policy abstraction but plan, own, reconcile, and recover each remote object separately. |
| Organization settings | Membership, SSO, global privacy, Relay, integrations, repositories, and feature flags | Saved enhanced privacy, required scrubbers, IP removal, global fields, and global advanced PII; inspected SSO, Relay, integrations, repositories, teams, members, developer settings, and Audit Log | Keep organization administration outside `SentryConnection` unless a narrow privacy baseline is separately approved. |

## Implementation order

1. Introduce the typed effective issue/metric representation and the modern
   detector/workflow/link plan without changing the public single-policy reference.
2. Make PagerDuty severity and status mapping explicit; add old/new adapter contracts and live UI
   acceptance for team notification and PagerDuty.
3. Add portable metric environment, interval, direction, high/medium/resolved thresholds, owner,
   description, actions, and throttle fields.
4. Add bounded common presets and an explicit compatibility matrix. Cover errors, spans, releases,
   and logs with read-after-write tests; reject Application Metrics until a metric selector can be
   validated against ingested catalog data.
5. Add typed issue triggers, match modes, filters, actions, owner, source scope, and throttle,
   starting with the common old/new subset and rejecting organization-wide scope.
6. Add modern workflow reuse, relinking, orphan cleanup, ambiguous-result recovery, and restart
   tests. Only then consider ordered policy composition.
7. Add project display name, hydration and chunk-load filters, bounded custom inbound filters, and
   dataset-specific advanced PII rules.
8. Add project email defaults only together with a supported email action. Evaluate source-fetching
   authentication and loader settings separately.
9. Decide separately whether a narrow organization privacy baseline belongs on
   `SentryConnection`; do not expand it into general organization administration.
10. Declare the three-core-CRD milestone complete only after create/update/disable/delete and
    restart acceptance on both old and new Sentry profiles.
11. Start `SentryCronMonitor` from the blueprint above, then `SentryUptimeMonitor` once the local
    acceptance profile includes a working Uptime checker. Generate both CRDs and RBAC from source.
12. Revisit Application Metrics ingestion fixtures, dynamic detection, Seer, ticket integrations,
    and arbitrary query syntax as compatibility-gated follow-up work.

## Verification requirements

Every implemented item must include:

- pure cross-field and compatibility validation before external I/O;
- adapter contract tests for every supported version;
- protected-state restart and interrupted-mutation tests;
- create, update, disable, and delete lifecycle coverage where applicable;
- read-after-write verification of the actual remote representation;
- live verification in the Sentry UI, Kubernetes conditions, and operator audit logs;
- generated CRDs and documentation updated from source types rather than edited by hand.

The legacy UI displayed a background-worker warning because the local acceptance stand uses the
project's reduced `errors-only` profile. This is a stand limitation, not an operator API
capability, and must not be encoded into the CRDs.
