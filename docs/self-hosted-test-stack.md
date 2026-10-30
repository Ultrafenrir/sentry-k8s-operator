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
make sentry-smoke SENTRY_VERSION=24.7.1
make sentry-status SENTRY_VERSION=24.7.1
make sentry-logs SENTRY_VERSION=24.7.1
make sentry-down SENTRY_VERSION=24.7.1
```

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
`/_health/`, and were removed with no managed containers or volumes left behind. Docker
exposed 15.6 GiB and 12 CPUs; the four local kind control-plane containers were stopped for
the sequential run and restored afterwards. The 24.7.1 run also confirmed that a missing
MaxMind license only stops the optional `geoipupdate` service. This is runtime-health
evidence, not authenticated Sentry API evidence, so it does not by itself raise any
capability in `docs/compatibility.txt` to `Verified`.

Allocate at least 14 GB to Docker Desktop before running the full matrix. CI runners need
the same resources plus enough disk for the official images. Put a job-level timeout around
`make sentry-matrix`; the upstream install phase can otherwise wait on a stalled container
runtime.

The installer deliberately skips interactive user creation. Local API-user/token
bootstrap and operator acceptance calls are a separate next step. A future SaaS token is
independent of these local stacks and must be supplied through a Secret or CI secret, never
committed to this repository.
