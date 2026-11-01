## Summary

Describe the problem and the scoped solution.

## Related context

Link the issue, ADR, design discussion, or compatibility evidence when applicable.

## Verification

List the exact commands run and their results.

```text
make verify-english
git diff --check
```

## Evidence boundary

State what was not tested. Distinguish source, fixture, envtest, kind, configuration-only,
event-ingestion, notification-delivery, and hosted-release evidence.

## Checklist

- [ ] The change is focused and contains no unrelated cleanup.
- [ ] Tests cover the changed contract and important failure paths.
- [ ] User-facing documentation and examples are updated.
- [ ] Generated CRDs, deepcopy code, RBAC, or Helm payloads were regenerated through repository tooling when required.
- [ ] No token, DSN, Secret data, protected-state content, external identity, customer payload, or raw sensitive response is included.
- [ ] `make verify-english` passes.
- [ ] `git diff --check` passes.
- [ ] Any unverified or external boundary is stated above.
