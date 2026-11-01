# Troubleshooting alpha resources

Start with Kubernetes status. Do not decode credential, binding, routing, or queue Secrets,
and do not paste raw Sentry responses into an issue.

```sh
kubectl get sentryconnection
kubectl -n <namespace> get sentryproject <name> -o json \
  | jq '{generation: .metadata.generation, observed: .status.observedGeneration, dsnClientKey: .status.dsnClientKey, conditions: .status.conditions, notificationTargets: .status.notificationTargets, alerts: .status.alerts}'
```

`metadata.generation != status.observedGeneration` means the latest spec has not completed
a reconciliation attempt. Wait for the controller or inspect its health before acting on an
older condition. Conditions and destination status contain fixed reasons and redacted
messages. The controller also emits a Kubernetes Event only when a condition's status,
reason, or fixed message changes; periodic reconciliation does not repeat unchanged Events.
This transition stream is useful context, not a complete audit trail.

```sh
kubectl -n <namespace> get events \
  --field-selector involvedObject.kind=SentryProject,involvedObject.name=<name>
```

## Connection conditions

- `SecretInputInvalid`: an administrator should verify that every referenced Secret exists
  in `sentry-kubernetes-operator-system` and contains the configured keys. Use `kubectl get
  secret <name>` without `-o yaml`, jsonpath, or base64 decoding.
- `ClientConfigurationInvalid`: inspect the public HTTPS URL and the referenced CA bundle
  configuration. Embedded URL credentials and plain HTTP are rejected by admission.
- `SecretInputsLoaded`: Kubernetes inputs loaded successfully. This does not prove Sentry
  reachability or API capabilities.

## Policy and readable references

- `AccessDenied`: the namespace, team, project observation, or DSN request is not granted by
  the selected `SentryConnection`. A platform administrator must review the matching access
  rule; the service owner must not weaken policy in the project CR.
- `PolicyAmbiguous`: more than one platform access rule matches the namespace. The
  administrator must make namespace ownership unambiguous.
- `DestinationAccessDenied`: the readable destination is outside the platform grant. No new
  destination mutation is authorized.
- `ReferenceNotFound`: correct the readable name or slug, or have the platform administrator
  make the resource available.
- `ReferenceAmbiguous`: duplicate readable candidates must be renamed or otherwise made
  unique. An internal identifier is not an accepted discriminator.
- `ManagedCreateUnsupported`: the target connection has not proved both atomic default-rule
  suppression and an automatic active client key. Do not force project creation.
- `ConnectionNotReady`: the selected connection has not loaded into the runtime registry;
  check its `CredentialsReady` condition.

## Safe retry conditions

`RateLimited`, `ServerError`, `Timeout`, `TransportError`, and `TemporaryFailure` are retried
with bounded delay. Existing verified DSN and protected identity are preserved on ambiguous
failures. Fix connectivity or credentials where appropriate and allow reconciliation to
retry; deleting the CR does not turn an unknown external outcome into a safe retry.

`Unauthorized` and `PermissionDenied` require an administrator to correct the Sentry token
or its scope. `Invalid`, `Conflict`, `NotFound`, and `DecodeError` require target-specific
investigation; do not infer that a missing response means a missing external object.

## Ownership and interrupted mutations

- `ProjectAlreadyExists`: the requested managed slug existed before this create intent and
  the connection policy does not authorize adoption of that exact unmarked project. The
  default is `Reject`; choose a different readable slug or use an explicitly granted
  read-only `project.use` resource. To take the existing slug under management, keep
  `project.create` and configure `Adopt` on an explicitly Active owned connection. `project.use`
  never becomes writable and is not an adoption request. `Adopt` is a platform policy, not a
  per-project force-takeover switch.
- `OwnershipConflict`: another SentryProject manager claim owns this project. Do not delete
  protected state or create a second writer.
- `RevisionUnverified`: the requested Active connection revision has not been verified
  against a remote ownership marker. The controller intentionally keeps the effective role
  Standby and blocks Sentry mutations; do not patch status to bypass the fence.
- `ConnectionStandby`: the selected connection is explicitly Standby. Increase the revision
  only as part of the authorized regional promotion workflow.
- `EffectiveRole=Standby`: the project requested a non-owning candidate role. A matching
  existing project is observed without a manager claim. Unmarked projects additionally require
  `projectManagement.unmanagedProjects: Standby`; only a candidate backed by protected
  managed-project evidence may contribute separately owned alerts.
- `OwnershipLost`, `ExternalObjectReplaced`, or `ExternalDeleted`: pinned identity/marker
  evidence no longer matches. Stop mutations and capture an administrator report.
- `ExternalNameChanged`: identity remains pinned but its readable name changed externally.
  Reconcile the external rename deliberately; the operator will not switch by name.
- `CreationOutcomeUnknown`: a create may have reached Sentry but cannot be proven. Automatic
  retry is blocked to prevent duplicates.
- `DeletionPending`: guarded deletion has not been verified complete. Finalizers are not
  removed by timeout or permission failure.

For the protected-state inventory:

```sh
kubectl sentry admin-report --context <context> > admin-report.json
```

The command is read-only and redacted. Follow
[`admin-recovery.txt`](admin-recovery.txt); there is intentionally no alpha `repair`,
`adopt`, or `force-delete` command.

## DSN conditions

- `DSNKeyAmbiguous`: multiple active keys exist. Set `spec.dsn.clientKey` to one exact,
  unique readable label.
- `DSNKeyNotFound`: the requested readable key label is absent or inactive.
- `DSNKeyNameInvalid`: Sentry returned an empty, overlong, or control-bearing readable key
  name. The working Secret is preserved and the unsafe value is not published.
- `ExternalNameChanged` with `DSNReady=True`: the exact pinned key was renamed in Sentry.
  Status shows its current readable name; the protected identity and working Secret remain
  current. Change `spec.dsn.clientKey` explicitly only if the new name should become the
  selector baseline.
- `DSNKeyRevoked`: the previously pinned active key was proven absent/inactive and the
  owned output Secret was removed.
- `DSNSecretConflict`: the requested Secret name is owned by something else. Do not
  overwrite it; select another `spec.dsn.secret` name after reviewing workload references.
- `AuthorizationChanged`: authorization changed between planning and the protected write.
  Reconcile the platform policy first, then let the controller retry.
- `DSNCutoverPending`: a renamed Secret is ready, but the old one is retained. Update the
  workload and then set annotation
  `sentryconfiguration.io/confirm-dsn-cutover=<new-secret-name>` on the SentryProject.
- `DSNAccessRevoked`: current policy denial was verified and owned output was removed.

Never print the value of `SENTRY_DSN`; Secret existence and condition state are sufficient
for routine diagnosis.

## Settings and destinations

- `SettingsInvalid` or `SettingsUnsupported`: correct only the typed alpha settings fields.
  Raw Sentry payloads and unlisted filters are not a workaround.
- `SettingsPartiallyApplied`: at least one independent settings component succeeded and was
  preserved while another failed. Inspect the component conditions before changing desired
  values.

## Alerts

- `AlertsNotReady` with per-rule `AlertAdapterUnavailable`: the connection is missing
  `spec.compatibility.alerts`, the selected profile does not support the trigger, or a
  legacy rule requests `enabled: false`. Correct the platform-owned profile or desired rule;
  do not use DELETE as a pause workaround.
- `AlertUpdateUnsupported`: the requested trigger-family change cannot preserve identity.
  Delete the old local key in one verified reconciliation, then create the new family under
  a deliberate key; the operator never performs an implicit delete/recreate.
- `AlertOwnershipLost` or `AlertMissing`: protected identity or marker evidence no longer
  matches Sentry. Do not recreate by hand; use the redacted administrator report.
- `AlertsPartiallyApplied`: independent rules that verified successfully were persisted;
  inspect only the local `status.alerts` state/reason entries for the failed rules.

Alert status never contains Sentry rule IDs, detector/workflow IDs, integration IDs,
desired hashes, or ownership markers.
- `DestinationUnsupported`: that adapter/catalog is not implemented or proven for the
  selected target. A rendered destination is not evidence of runtime support.
- `DestinationAccessRevoked`: new operations are denied and cleanup state is retained.
- `DestinationCleanupPending`: exact route/resource cleanup has not completed; do not delete
  routing records by hand.
- `TelegramRegistrationRequired`: complete the one-time authenticated registration flow;
  numeric chat/topic identities never belong in the CR.

## Controller health

Check both replicas, probes, PDB and the current Lease holder without reading Secret data:

```sh
kubectl -n sentry-kubernetes-operator-system get deploy,pod,pdb
kubectl -n sentry-kubernetes-operator-system get lease c4d7095e.io -o json \
  | jq '{holder: .spec.holderIdentity, renewTime: .spec.renewTime}'
```

Controller logs are designed to omit credentials, DSNs and response bodies, but they are
still operational data. Restrict access and redact local names before external sharing.

Prometheus exposes the current number of observed projects per synchronization mechanism
without namespace, resource-name, Sentry-ID, or error-message labels:

```promql
sum by (mechanism) (sentry_operator_sync_resources{state="blocked"})
```

The fixed mechanisms are `project`, `dsn`, `settings`, `destinations`, and `alerts`; states
are `ready`, `blocked`, and `inactive`. `SentryOperatorSynchronizationBlocked` fires when a
mechanism remains blocked for ten minutes. Use the metric to select the failing mechanism,
then use the redacted Kubernetes conditions and Events above to locate the resource and
reason.
