# Remaining development plan

This file contains only work that is not complete. The implemented five-CRD surface and current
acceptance contract are documented in `docs/project-status.md`, `docs/configuration.md`, and
`docs/acceptance-runbook.md`.

## Bounded alert capabilities

### Dynamic detection and Seer

- [ ] Run create, update, disable, delete, interrupted-mutation, restart, and browser acceptance with
  the required Seer services enabled.

  The pinned self-hosted Sentry 26.10.1 distribution does not provide a Seer service. Do not replace
  this gate with a mock: completion requires an approved Seer-enabled acceptance environment.

Every completed item must include pure pre-I/O validation, adapter contracts, protected-state and
restart coverage, exact read-after-write verification, browser evidence for every visible public
setting, safe status and logs, generated artifacts, English documentation, `make test`, `make
lint`, `make verify-english`, and `git diff --check`.
