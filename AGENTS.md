# Repository agent guide

This file is the mandatory starting point for any coding agent working in this repository.
Read it before running commands or changing files.

## Language policy

- Write all repository documentation, source comments, test comments, generated guidance,
  commit messages, and user-facing command output in English.
- Existing public API names and external product terms are not translated.
- Conversation with the repository owner may use another language, but repository content
  must remain English-only.

## First five minutes

1. Run `git status --short` and preserve every pre-existing change. Never discard or
   overwrite work merely because the tree is dirty.
2. Read `README.md`, `docs/development-runbook.md`, and `docs/project-status.md`.
3. Run `make help` before inventing a new command. Prefer the checked-in Make targets and
   scripts because they encode pinned versions and cleanup boundaries.
4. Check the relevant progress or compatibility document before claiming a feature works.
5. Run `make verify-english` and `git diff --check` before handing work back.

## Repository map

- `api/v1alpha1/`: public CRD types and validation markers.
- `internal/`: controllers and domain implementations.
- `config/`: canonical Kustomize resources, samples, and the local demo manifests.
- `charts/`: generated Helm chart; regenerate it instead of editing generated payloads by
  hand.
- `hack/`: verification and local-environment scripts used by Make targets.
- `testdata/http-fixtures/`: sanitized Sentry contracts; fixtures are not live evidence.
- `docs/development-runbook.md`: clean local Sentry + kind + operator procedure.
- `docs/project-status.md`: implemented, verified, intentionally deferred, and blocked work.
- `docs/self-hosted-test-stack.md`: exact self-hosted Sentry lifecycle and evidence limits.

## Safe working rules

- Use Git for repository inspection (`git status`, `git diff`, `git log`). Do not use a
  browser or a hosting UI for ordinary Git work.
- Do not commit API tokens, DSNs, bot tokens, integration keys, passwords, raw Secret data,
  or protected-state contents. Local Sentry tokens belong under the ignored
  `.cache/sentry-self-hosted/tokens/` directory.
- Never run Docker-wide prune commands. The Sentry wrapper removes only its version-scoped
  Compose projects and volumes. Delete only an explicitly named kind cluster.
- Do not modify or delete unrelated containers, volumes, images, clusters, or user files.
- Do not claim browser, SaaS, notification-delivery, public-DNS, certificate, registry, or
  release evidence unless that exact path was exercised successfully.
- Do not treat sanitized fixtures, source inspection, mocks, envtest, or configuration CRUD
  as external delivery evidence.
- `config/demo/kind-local.yaml` intentionally contains resources that remain not ready on
  the local stack. Read the expected-state table in the runbook before treating that as a
  defect.

## Local environment defaults

- Persistent development cluster: `sentry-operator-dev` / context
  `kind-sentry-operator-dev`.
- Lightweight Sentry: version `26.10.1`, profile `errors-only`, URL
  `http://127.0.0.1:26101`.
- Legacy alert API acceptance: Sentry `24.7.1`, profile `feature-complete`, URL
  `http://127.0.0.1:24710`.
- Operator namespace: `sentry-kubernetes-operator-system`.
- Local demo organization/team: `sko-live-26-10-1` / `sko-live`.
- Local demo manifests: `config/demo/kind-local.yaml` and
  `config/demo/kind-local-readonly.yaml`.

Plain HTTP and TLS verification bypass are separate options. `allowInsecureHTTP: true`
permits an `http://` URL. `insecureSkipTLSVerify: true` keeps HTTPS encryption but skips
certificate and hostname verification. Neither belongs in production.

## Required verification discipline

Choose the smallest gate that proves the change, then expand in proportion to risk:

- Documentation or shell-only changes: `make verify-sentry-self-hosted` when relevant and
  `make verify-english`, then `git diff --check`.
- API/CRD changes: `make verify-t01-generated verify-samples`.
- Controller/domain changes: the matching `verify-tNN-*` target, then `make test` or
  `make test-matrix` when practical.
- Helm/Kustomize changes: `make verify-helm` plus the matching render or kind gate.
- Self-hosted Sentry changes: follow `docs/self-hosted-test-stack.md`; state whether the run
  was configuration-only, ingestion, or delivery evidence.

Record the exact commands run and any unverified boundary in the final handoff. A passing
configuration-only run must never be described as event ingestion or notification delivery.

## Important local-stack facts

- `make sentry-up` is destructive only for the selected version-scoped Sentry stack: it
  recreates that stack from empty volumes.
- On Docker Desktop, registry credential lookup may hang. If every rendered image is already
  cached, retry with `SENTRY_SKIP_PULL=true`; the wrapper fails closed if an image is absent.
- `SentryConnection` loads credentials and transport settings but does not discover or
  promote server capabilities. The disposable demo therefore applies explicit, previously
  proven capability status as documented in the runbook.
- Sentry 26.10.1 supports the modern detector/workflow path used by the demo. Its legacy
  `/rules/` and `/alert-rules/` collection routes return 404; legacy live acceptance belongs
  on Sentry 24.7.1.
- The lightweight 26.10.1 profile is suitable for project, key, settings, filter, and modern
  alert configuration checks. It is not feature-complete ingestion or delivery evidence.
- Local PagerDuty data is a disposable catalog fixture. Slack remains unsupported without a
  version-compatible channel catalog. Telegram requires registration and real credentials.

## Definition of done for a work session

- The requested implementation or documentation is present.
- Relevant generated files are updated through repository tooling.
- Focused tests pass, or the exact blocker and unrun tests are stated.
- `git diff --check` passes.
- `make verify-english` passes.
- `git status --short` is reviewed, with unrelated pre-existing changes preserved.
- `docs/project-status.md` is updated when the project evidence boundary or backlog changes.
