# Local self-hosted Sentry test stacks

The repository wraps the official `getsentry/self-hosted` Compose distribution instead of
maintaining a forked Sentry stack. The wrapper accepts only these exact tag/commit pairs:

| Sentry | Self-hosted commit | Compose project | Local URL |
| --- | --- | --- | --- |
| 24.7.1 | `f330f3040d3ab81055f92b4ac4265fbbfe17487c` | `sko-sentry-24-7-1` | `http://127.0.0.1:24710` |
| 26.10.1 | `192a826421decd7f8ce6673bb9a299737d0d6916` | `sko-sentry-26-10-1` | `http://127.0.0.1:26101` |

## Fresh-state contract

`make sentry-up SENTRY_VERSION=<version>` always performs the following sequence:

1. Remove every container, network, Compose-managed volume, and official external volume
   belonging to the selected version-scoped project.
2. Delete only the checkout carrying this repository's exact management marker.
3. Clone the official tag again and verify its commit SHA.
4. Patch only the local generated checkout: select a version-scoped Compose project,
   bind Sentry to localhost, set one-day event retention, and replace upstream's global
   external volume names with version-scoped names.
5. Render and inspect Compose before installation. Any remaining global persistent Sentry
   volume name fails the run.
6. Run the official non-interactive installer, start Compose with `--wait`, and require a
   successful `/_health/` response.

The official stack runs `geoipupdate` as an optional one-shot job. Without administrator-
provided MaxMind credentials it exits with code 1 after installing an empty local database.
The readiness gate accepts only that exact optional exit and still rejects any other
stopped, restarting, unhealthy, or not-yet-healthy service before checking `/_health/`.

The generated checkout lives under `.cache/sentry-self-hosted/` and is ignored by Git.
The wrapper refuses to replace or remove a checkout without the exact management marker.
It never invokes Docker-wide prune operations and does not remove kind clusters, unrelated
images, containers, networks, or volumes.

The two versions intentionally cannot run concurrently. The official distributions build
several local images with shared names, so the matrix starts, checks, and removes them
sequentially.

## Commands

Verify the local wrapper without fetching or starting Sentry:

```sh
make verify-sentry-self-hosted
```

Fetch an exact official tree and validate its fully rendered Compose configuration:

```sh
make verify-sentry-compose SENTRY_VERSION=24.7.1
make verify-sentry-compose SENTRY_VERSION=26.10.1
```

Start one clean feature-complete stack, inspect it, and remove all of its state:

```sh
make sentry-up SENTRY_VERSION=24.7.1
make sentry-bootstrap SENTRY_VERSION=24.7.1
make sentry-live-config-api SENTRY_VERSION=24.7.1
make sentry-live-api SENTRY_VERSION=24.7.1
make sentry-smoke SENTRY_VERSION=24.7.1
make sentry-status SENTRY_VERSION=24.7.1
make sentry-logs SENTRY_VERSION=24.7.1
make sentry-down SENTRY_VERSION=24.7.1
```

`make sentry-bootstrap` creates or updates the fixed local acceptance user and its
version-scoped organization/team fixture through Sentry's internal provisioning service,
creates a deliberately invalid GitHub integration plus one attached repository for portable
source-code catalog and code-mapping tests, and creates a deliberately invalid PagerDuty
integration/service catalog entry whose routing key is the literal
`invalid-routing-key-not-for-delivery`; its synthetic numeric service ID exists only to
satisfy the Sentry workflow form's integer contract. Bootstrap replaces only its managed
user token, verifies Bearer authentication against
`/api/0/organizations/`, and stores the token at
`.cache/sentry-self-hosted/tokens/<version>.token` with mode `0600`. The token directory
is ignored by Git. A successful `sentry-live-config-api` or `sentry-live-api` run also writes
a mode-0600 evidence record containing only the exact version, organization, successful
gate, and proven safe-create capabilities. The bounded kind demo validates this record before
applying connection status. `sentry-down`, `sentry-reset`, and `sentry-clean` remove both the
managed token and capability evidence together with the selected runtime.

`make sentry-live-api` is destructive and is intended only for the disposable local stack.
It verifies the bootstrapped organization/team and creates a marker-bearing project through
Sentry's public HTTP API, omits `platform`, sends `default_rules=false`, verifies the project
round-trip and the single active Default client key, confirms on 24.7.1 that no legacy
default rule was created, deletes the project, and requires it to become unreadable. On
26.10.1 that alert effect remains source/contract-tested because only POST, not GET, is
published on the project-detectors endpoint. On both versions it also updates and reads
back all five published inbound filters, all five typed PII fields, all four typed behavior
fields, the team-only issue-ownership contract, aliased source-code catalog resolution,
code-mapping create/update/delete, and Sentry CODEOWNERS child storage create/delete. The
invalid local integration cannot fetch a real provider file, so this is not external
CODEOWNERS import evidence. On 24.7.1
it verifies legacy issue and metric alert create/read/update/delete lifecycles; on 26.10.1
it verifies workflow create/read/update/disable/enable/delete plus an attached events/count()
metric detector create/read/disable/enable/delete lifecycle without external actions.
On both versions it posts a synthetic event through the active Default DSN, waits for the
exact event ID through the public event-detail API, requires both configured sensitive
extras to be `[Filtered]`, preserves a non-sensitive control value, and rejects any response
that still contains either original sentinel.
It then posts a second event with `user.ip_address=127.0.0.1` while the localhost filter is
active and requires the exact event ID to remain unreadable. Matching browser-extension and
web-crawler control events must be readable with their filters disabled. Acceptance creates
a separate disposable project, enables both filters before its first ingestion so Relay
cache timing cannot affect the result, and requires both matching event IDs to remain 404.
A separate project-create request deliberately discards its successful response; acceptance
recovers it only through GET by slug plus the exact marker, without a second POST, and deletes
all three projects. The following controller acceptance passes the create through a local TLS
fault proxy and closes the connection after Sentry accepts it. A real envtest Kubernetes API
stores the Submitted operation in a protected Secret; a newly constructed store, API client,
and engine then recover the exact marker by GET without a second POST, clear the journal, and
complete guarded deletion of the Sentry project and binding. A second full-controller case
creates a managed project and Kubernetes binding/claim/DSN Secret, introduces external drift
into all five filters, five PII fields, five behavior fields, and issue ownership, and
requires one reconcile to restore them with exactly five filter PUTs, two independently
narrow project PUTs, and one ownership PUT. A following reconcile must perform no settings
or ownership mutation, SettingsReady, IssueOwnershipReady, and Ready must remain true, and
guarded deletion must complete. The same case creates an additional
authorized team, grants it project access,
verifies `GrantAccessReady`, removes the desired grant, and requires the additional team to
be revoked while the primary project team remains attached. The matrix runs both acceptance
layers after bootstrap and before removing the stack.

The same full-controller case resolves the stable connection integration/repository aliases,
creates one protected code mapping, introduces visible path drift, requires the next
reconcile to restore it, confirms mutation-free convergence, and deletes only the exact
pinned mapping during explicit-empty cleanup. Public status is checked for safe local state
only, while external integration, repository, mapping, and CODEOWNERS identities remain in
the protected binding.

One live ownership case creates an unmarked project through the public API, observes it as an
External/ReadOnly standby candidate without a claim, proves a plain Active request remains
blocked, then adopts it under an explicitly Active connection and `unmanagedProjects: Adopt`.
It verifies the exact identity and regional evidence before applying Retain. A second case
creates a managed project, observes the same remote identity through a managed standby
candidate, proves promotion is blocked without authorization, consumes an exact handoff,
and promotes without replacing the remote project. Its Retain cleanup removes the marker and
protected claim/binding while leaving the same external project readable. These are
configuration-only ownership lifecycle checks, not failover or external delivery evidence.
An independent full-controller case creates a user-count alert owned by a managed standby
candidate. Candidate Retain must remove the exact legacy rule or modern detector/workflow
before deleting the read-only binding, while the manager claim, project identity, and regional
evidence remain unchanged. The disposable PagerDuty catalog supplies configuration identities
only; this case does not send a notification.

`make sentry-live-config-api` runs the same public API project, key, settings, filter and
alert object lifecycle checks, but intentionally skips event ingestion and filter-behavior
events. This is the correct gate when validating configuration creation without external
delivery or when Relay rejects envelopes because a resource-constrained Docker VM is above
its memory health threshold. It must not be reported as ingestion, PII/filter behavior, or
notification-delivery evidence.

`make sentry-api-report SENTRY_VERSION=<version>` is a read-only local inventory. It lists
project names/slugs/platforms, client-key names/active state/rate limits, filter state,
PII booleans and field counts, and the selected legacy-rule or modern detector/workflow
summary. Its fixed projections omit external object IDs, DSNs, keys, tokens, routing keys,
raw Relay configuration, and sensitive/safe field names. The report requires the managed
mode-0600 local token and is not a general SaaS inventory command.

`make sentry-clean` additionally removes the selected managed upstream checkout.
`make sentry-matrix` performs the clean start/smoke/removal cycle for both versions in
order and uses a cleanup trap if a check fails.

Useful overrides are `SENTRY_PORT`, `SENTRY_WAIT_TIMEOUT`, and
`SENTRY_SELF_HOSTED_CACHE` (which must be an absolute, non-broad path). The profile defaults
to `feature-complete`. `SENTRY_PROFILE=errors-only` is an explicit lower-resource smoke
profile and must not be used as evidence for the complete alert/workflow surface.

If Docker registry pulls are temporarily unavailable but every image in the rendered
Compose model is already present locally, `SENTRY_SKIP_PULL=true` bypasses only the upstream
image-update step. The wrapper verifies the complete rendered image set before invoking the
installer and fails closed when any image is missing. The default remains `false`.

The current installer requires Bash 4.4 or newer. The wrapper automatically prefers
Homebrew Bash at `/opt/homebrew/bin/bash` or `/usr/local/bin/bash`, then checks the Bash on
`PATH`. CI can select another absolute executable with `SENTRY_BASH_BIN`; macOS's system
`/bin/bash` 3.2 is intentionally rejected and never replaced.
The wrapper also removes the upstream installer's hard-coded `default` Buildx builder so
Docker Compose uses the builder belonging to the active context. This is required by Docker
Desktop, whose active context and builder are normally named `desktop-linux`.

## Resource and evidence boundary

The preflight enforces the official minimums before running the installer:

| Version/profile | Docker memory | Docker CPUs |
| --- | ---: | ---: |
| 24.7.1 feature-complete | 3.8 GB | 2 |
| 26.10.1 errors-only | 7 GB | 2 |
| 26.10.1 feature-complete | 14 GB | 4 |

On 2026-10-30 the clean 24.7.1 feature-complete stack completed its upstream migrations and
became API-ready on a Docker Desktop VM exposing 15.6 GiB and 12 CPUs. Its configuration-only
gate passed project/default-rule/key/settings plus legacy issue and metric alert
create/read/update/delete, followed by controller lost-response recovery and settings drift
repair. The same run exposed an important ingestion boundary: Relay reported system memory
above its 95% health threshold and rejected the first synthetic envelope with 503. This is
configuration lifecycle evidence only, not event ingestion, filter behavior, or delivery
acceptance.

On 2026-10-31 a new clean 24.7.1 feature-complete run repeated the configuration-only API
and controller gates successfully. It also verified additional-team grant, read-back,
revoke, and primary-team preservation through the real public project-team endpoints. Event
ingestion was intentionally skipped, so this run adds configuration evidence only.

A clean 26.10.1 feature-complete attempt completed the upstream ClickHouse and PostgreSQL
migrations, but the web and Snuba API health checks did not stabilize within the practical
10–12 minute local limit. Snuba initialization took 479 seconds under memory pressure. The
attempt was stopped and all version-scoped volumes were removed instead of waiting for the
30-minute emergency timeout.

The subsequent clean 26.10.1 `errors-only` run became fully healthy. It created the managed
local acceptance user and token, authenticated against the API, and passed real
project/default-rule/key/settings CRUD plus attached modern metric detector/workflow
create/read/update/toggle/delete acceptance. The live controller
also passed lost-response recovery and settings-drift repair. The token, runtime, and every
managed volume were removed afterwards. `errors-only` is sufficient evidence for these
configuration APIs, but it must not be represented as feature-complete ingestion or delivery
acceptance. Detector families beyond the verified metric detector, external actions,
integrations, delivery, and SaaS remain unverified.

On 2026-10-31 the clean 26.10.1 `errors-only` run was repeated after the project-access
change. The configuration-only API and controller gates passed, including additional-team
grant, read-back, revoke, and primary-team preservation. The version-scoped runtime, token,
and managed volumes were removed afterwards.

Later on 2026-10-31 clean configuration-only runs on 24.7.1 `feature-complete` and 26.10.1
`errors-only` accepted the expanded settings contract. Both public API runs updated and read
back filtered-transaction, the selected legacy-browser subfilters, auto-resolve hours,
default environment, crash-report retention, JavaScript/source-map scraping, and allowed
browser origins. Both live controller runs repaired drift across all five filters, all five
PII fields, and all five
behavior fields with five filter PUTs plus two independent project PUTs, then converged to a
mutation-free reconcile. Both version-scoped runtimes, tokens, and managed volumes were
removed afterwards. These runs add configuration reconciliation evidence only.

The next clean configuration-only runs on both targets accepted the team-only
`issueOwnership` contract through the public project ownership endpoint. Direct API checks
updated and read back structured path and tag rules, fallthrough, auto-assignment, and
CODEOWNERS synchronization. Each live controller run then repaired external ownership drift
with exactly one ownership PUT and converged to a mutation-free reconcile with
`IssueOwnershipReady=True`. Both version-scoped runtimes, tokens, and managed volumes were
removed afterwards. These runs do not prove event-driven assignment or CODEOWNERS import.

The following clean configuration-only runs on 24.7.1 `feature-complete` and 26.10.1
`errors-only` accepted the managed client-key lifecycle through both the direct public API
gate and the full controller. Each target created a server-generated key, updated active
state and count/window rate limits, created a rotation replacement, cut the owned output
Secret over to the replacement public DSN, guarded deletion by exact identity and ownership
label, and removed the remaining managed key. Secret key material and private DSNs were not
printed or persisted by the acceptance code. Both version-scoped runtimes and managed
volumes were removed afterwards. These runs prove configuration lifecycle only; event
ingestion and notification delivery were intentionally skipped.

The next clean configuration-only runs on the same two targets accepted the custom
symbol-source lifecycle through both the direct public API gate and the full controller.
Each target created HTTP, S3, and GCS sources with fake credentials, returned redacted secret
fields, rotated the HTTP password, and removed the exact three identities. The controller
loaded credentials from a namespace-local Secret, persisted no credential material in status
or protected bindings, repaired deliberate visible HTTP drift and a Secret rotation with
exactly one source update, converged without another mutation, and cleaned up every exact
owned source. Both version-scoped runtimes, tokens, and managed volumes were removed
afterwards. These runs prove configuration lifecycle only; symbol download, native
symbolication, event ingestion, and notification delivery were intentionally skipped.

Legacy issue and metric alert routes are evidenced only by the 24.7.1 run above. A live
26.10.1 stack returns `404` for the legacy `/rules/` and `/alert-rules/` collection routes;
pinning `legacy-24.7.1` compatibility against that server must remain visibly not ready.

For a full ingestion run, stop other Docker workloads or allocate enough headroom that Relay
remains below its memory health threshold. For 26.10.1 feature-complete, the 14 GB preflight
minimum is necessary but not sufficient on a 15.6 GiB Docker VM.

Allocate at least 14 GB to Docker Desktop before running the full matrix. CI runners need
the same resources plus enough disk for the official images. Put a job-level timeout around
`make sentry-matrix`; the upstream install phase can otherwise wait on a stalled container
runtime.

The installer deliberately skips interactive user creation; `sentry-bootstrap` supplies
the ephemeral local acceptance identity described above. A future SaaS token is independent
of these local stacks and must be supplied through a Secret or CI secret, never committed
to this repository.
