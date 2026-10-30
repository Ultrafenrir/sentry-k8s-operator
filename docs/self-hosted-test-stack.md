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
make sentry-live-api SENTRY_VERSION=24.7.1
make sentry-smoke SENTRY_VERSION=24.7.1
make sentry-status SENTRY_VERSION=24.7.1
make sentry-logs SENTRY_VERSION=24.7.1
make sentry-down SENTRY_VERSION=24.7.1
```

`make sentry-bootstrap` creates or updates the fixed local acceptance user and its
version-scoped organization/team fixture through Sentry's internal provisioning service,
replaces only its managed user token, verifies Bearer authentication against
`/api/0/organizations/`, and stores the token at
`.cache/sentry-self-hosted/tokens/<version>.token` with mode `0600`. The token directory
is ignored by Git. `sentry-down`, `sentry-reset`, and `sentry-clean` remove the managed
token file together with the selected runtime.

`make sentry-live-api` is destructive and is intended only for the disposable local stack.
It verifies the bootstrapped organization/team and creates a marker-bearing project through
Sentry's public HTTP API, omits `platform`, sends `default_rules=false`, verifies the project
round-trip and the single active Default client key, confirms on 24.7.1 that no legacy
default rule was created, deletes the project, and requires it to become unreadable. On
26.10.1 that alert effect remains source/contract-tested because only POST, not GET, is
published on the project-detectors endpoint. On both versions it also updates and reads
back the three published inbound filters and all five typed PII settings fields. On 24.7.1
it verifies legacy issue and metric alert create/read/update/delete lifecycles; on 26.10.1
it verifies detached workflow create/read/update/disable/enable/delete.
The matrix runs this acceptance after bootstrap and before removing the entire stack.

`make sentry-clean` additionally removes the selected managed upstream checkout.
`make sentry-matrix` performs the clean start/smoke/removal cycle for both versions in
order and uses a cleanup trap if a check fails.

Useful overrides are `SENTRY_PORT`, `SENTRY_WAIT_TIMEOUT`, and
`SENTRY_SELF_HOSTED_CACHE` (which must be an absolute, non-broad path). The profile defaults
to `feature-complete`. `SENTRY_PROFILE=errors-only` is an explicit lower-resource smoke
profile and must not be used as evidence for the complete alert/workflow surface.

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

On 2026-10-30 both pinned feature-complete stacks were installed from fresh volumes on the
local Docker Desktop VM, completed their upstream migrations, became healthy, answered
`/_health/`, and were removed with no managed containers or volumes left behind. Both
versions also created the managed local acceptance user and user token and successfully
authenticated a Bearer request to `/api/0/organizations/`; token mode and removal were
verified. Docker exposed 15.6 GiB and 12 CPUs; the four local kind control-plane containers
were stopped for the sequential runs and restored afterwards. The 24.7.1 run also
confirmed that a missing MaxMind license only stops the optional `geoipupdate` service.
Both versions also passed real project create/read/key/delete acceptance: platform was
omitted, the marker round-tripped, one active Default key had a local DSN, and deletion
became a 404. The 24.7.1 run additionally observed zero default legacy rules. Settings,
filters and PII fields passed HTTP round-trip on both versions; synthetic event semantics,
detector/action lifecycle, integrations, delivery, lost-response recovery and SaaS remain
unverified. Legacy issue/metric CRUD and detached modern workflow toggle/lifecycle are verified.

Allocate at least 14 GB to Docker Desktop before running the full matrix. CI runners need
the same resources plus enough disk for the official images. Put a job-level timeout around
`make sentry-matrix`; the upstream install phase can otherwise wait on a stalled container
runtime.

The installer deliberately skips interactive user creation; `sentry-bootstrap` supplies
the ephemeral local acceptance identity described above. Settings and alert lifecycle
acceptance calls are the next step. A future SaaS token is independent of these local
stacks and must be supplied through a Secret or CI secret, never committed to this repository.
