# Repository agent guide

Repository content must be written in English. Preserve pre-existing worktree changes.

Before changing files, run `git status --short`, read `README.md`,
`docs/development-runbook.md`, and `docs/project-status.md`, then run `make help`.

Use generated artifacts rather than editing generated CRDs or RBAC by hand. Before handoff run
the smallest relevant tests, `make verify-english`, and `git diff --check`. Never commit tokens,
DSNs, passwords, raw Secret data, or protected controller state.
