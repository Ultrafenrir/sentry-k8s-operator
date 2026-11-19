# Upgrade and rollback policy

The API is `v1alpha1`, but stored objects and protected controller state are still data contracts.
An upgrade must apply generated CRDs before starting the new controller. Prefer compatible field
additions; any rename, removal, or semantic change requires a conversion or an explicit migration
design before release. The `SentryProject` change below is such an explicit breaking alpha
migration.

Protected bindings are versioned independently of CRDs. A new controller may read the current
format and explicitly supported older formats, then persist the new format only after the bound
remote identity and ownership evidence have been verified. It must never recover by name alone.
Back up the operator namespace and application CRs before an upgrade, but treat Secret data and
protected state as confidential.

The supported upgrade procedure is:

1. Confirm all resources have current observed generations and no deletion is in progress.
2. Back up CRs, the operator namespace, and the Helm release using the cluster's protected backup
   process.
3. Render and inspect the new chart, then run `helm upgrade`; do not hand-edit CRDs or RBAC.
4. Verify `SentryConnection`, every consuming policy and project, and independent monitors in that
   order.
5. Compare protected identity counts without printing identities, inspect logs, and verify the
   existing Sentry objects in the browser before allowing new mutations.

A binary rollback is supported only when the previous controller understands every installed CRD
field and protected binding version, and the failed upgrade did not complete a one-way remote
mutation. Rolling back only the Deployment while leaving a newer incompatible CRD or binding format
is unsafe. When those conditions are not provable, keep the new CRDs, stop mutations, and restore
through the documented protected backup process instead of deleting state or adopting by name.

Promotion beyond `v1alpha1` requires continuous upgrade and rollback acceptance from the previous
release, conversion coverage for every stored version, and a compatibility policy for at least one
older controller release.

## SentryProject alpha API migration

The current alpha replaces `spec.project.create` and `spec.project.use` with a single ensure-style
contract. Before starting the new controller, rewrite every project so the Sentry slug is
`metadata.name`, move `team`, `platform`, and `displayName` directly under `spec`, and remove the
project-level `role`. The referenced `SentryConnection.spec.ownership` now supplies the only
Active/Standby decision.

Replace `spec.dsn.managedClientKey` by setting `publishDSN: true` on exactly one matching
`spec.clientKeys` entry. Replace `spec.dsn.clientKey` with `spec.existingClientKey`. A project that
uses the normal single active Sentry Client Key needs neither block. Apply the generated CRD, apply
the migrated resources while the old controller is stopped, and only then start the new
controller. Verify ownership evidence and remote identity before permitting adoption or deletion.

The current release renames the namespace DSN output Secret from `sentry-dsns` to `sentry-dsn`
and creates the new Secret automatically. During an upgrade, leave the legacy Secret in place
until every project is Ready and the corresponding key exists in `sentry-dsn`. The controller does
not migrate or delete the legacy Secret because it cannot prove that no older controller or
consumer still uses it. Remove `sentry-dsns` through the application's normal Secret-management
process only after consumers have switched to `sentry-dsn`.

`make verify-upgrade` automates the current compatibility lane from commit
`9567f58b6c7361f6701bbdbce60772c7d6437c32` (chart `0.2.0-alpha.1`) to the current chart. It uses an
isolated kind cluster, verifies remote and protected-state counts after every transition, rejects
unsafe controller log evidence, exercises guarded deletion, and restores the original kubectl
context. Change `PREVIOUS_REF` only when cutting a release whose immediate supported predecessor is
different.

