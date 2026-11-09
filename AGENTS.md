# Repository agent guide

Repository content must be written in English. Preserve pre-existing worktree changes.

Before changing files, run `git status --short`, read `README.md`,
`docs/development-runbook.md`, and `docs/project-status.md`, then run `make help`.

The product surface is limited to the `SentryConnection`, `SentryProject`, and
`SentryAlertPolicy` CRDs and their controller. The default Helm chart installs the operator and a
connection; application teams own project and policy instances.

Use generated artifacts rather than editing generated CRDs or RBAC by hand. Before handoff run
the smallest relevant tests, `make verify-english`, and `git diff --check`. Never commit tokens,
DSNs, passwords, raw Secret data, or protected controller state.
