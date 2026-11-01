# Contributing

Thank you for helping improve Sentry Kubernetes Operator. The project welcomes focused bug
reports, documentation corrections, tests, design discussion, and code contributions that
fit the published alpha boundary.

By participating, you agree to follow the [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). By
submitting a contribution, you represent that you have the right to submit it and agree that
it is licensed under the repository's [Apache License 2.0](LICENSE), consistent with section
5 of that license.

## Before opening an issue

1. Read the [README](README.md), [configuration guide](docs/configuration.md), and current
   [project status](docs/project-status.md).
2. Search existing issues and pull requests for the same behavior.
3. Check the [support policy](SUPPORT.md) and [security policy](SECURITY.md).
4. Remove tokens, DSNs, Secret data, protected-state contents, external IDs, and customer
   payloads from every reproduction and log excerpt.

Use a public issue for reproducible bugs, feature proposals, documentation gaps, or usage
questions that contain no sensitive data. Use private vulnerability reporting for suspected
security issues.

## Development setup

Repository content must be written in English. Start with [`AGENTS.md`](AGENTS.md), which
defines repository safety and verification rules, then read
[`docs/development-runbook.md`](docs/development-runbook.md).

Inspect the available pinned workflows before inventing commands:

```sh
git status --short
make help
```

The main local prerequisites are Go, Git, Make, Docker, kubectl, Helm, and Bash 4.4 or newer.
Some tests also require kind, cert-manager, or a disposable self-hosted Sentry stack. The
checked-in Make targets install or verify project-pinned development tools where practical.

## Contribution workflow

1. Fork the repository and create a focused branch from the current `main` branch.
2. Keep a change small enough to review and test independently.
3. Add or update tests and user-facing documentation with the implementation.
4. Regenerate artifacts only through repository tooling.
5. Run the smallest verification gate that proves the change, then expand in proportion to
   risk.
6. Open a pull request using the repository template and describe every unverified boundary.

Do not combine unrelated refactors with a bug fix. Do not rewrite generated CRDs, Helm
payloads, or deepcopy code by hand.

## Generated files

The canonical sources are Go API markers and Kustomize manifests. Use:

```sh
make manifests generate
```

For Helm payload changes, update canonical Kustomize sources, then run:

```sh
make helm-generate
make verify-helm
```

Generated diffs must be committed with their source change. A pull request that edits only
generated payloads without changing the canonical source will not be accepted.

## Verification guide

Choose gates according to the affected surface:

| Change | Required starting checks |
| --- | --- |
| Documentation | `make verify-english`, `git diff --check` |
| API or CRD | `make verify-t01-generated verify-samples` |
| Controller or domain logic | Matching `verify-tNN-*` target, then `make test` or `make test-matrix` when practical |
| Helm or Kustomize | `make verify-helm` and the matching render or kind gate |
| Self-hosted Sentry | Follow `docs/self-hosted-test-stack.md` and state whether evidence is configuration-only, ingestion, or delivery |

Before requesting review, always run:

```sh
make verify-english
git diff --check
```

Do not describe fixtures, mocks, source inspection, envtest, configuration CRUD, or a
rendered manifest as live external delivery evidence.

## Code and documentation style

- Follow standard Go formatting and use `make fmt` or `gofmt` through the repository
  workflow.
- Prefer typed, bounded API contracts over raw vendor payloads or settings maps.
- Keep public status, logs, Events, and metrics free of credentials and external identities.
- Preserve unmanaged Sentry objects and existing user changes in the working tree.
- Write comments and documentation to explain contracts and safety boundaries, not obvious
  syntax.
- Use imperative, concise English commit subjects.

## API and compatibility changes

The CRDs are `v1alpha1`, but their public shape still requires deliberate evolution. A new
field must have stable readable semantics, validation, an ownership model, version-specific
adapter evidence, generated artifacts, admission tests, configuration examples, and an
updated compatibility/evidence record.

Read [`docs/alpha-surface.txt`](docs/alpha-surface.txt),
[`docs/compatibility.txt`](docs/compatibility.txt), and the ADRs before proposing API
expansion. Features that depend on external credentials or public infrastructure must state
that evidence boundary explicitly.

## Pull request expectations

A reviewable pull request includes:

- a clear problem statement and scoped solution;
- linked issue or design context when available;
- tests for success, failure, authorization, recovery, and redaction paths as applicable;
- regenerated artifacts and updated examples when public behavior changes;
- exact verification commands and results;
- explicit untested or external boundaries;
- no secrets, customer data, generated credentials, or protected-state contents.

Maintainers may ask for a design issue before accepting broad API, security, storage,
recovery, release, or compatibility changes. Acceptance and release decisions follow
[`GOVERNANCE.md`](GOVERNANCE.md).
