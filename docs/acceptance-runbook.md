# Physical acceptance runbook

Physical acceptance is a release gate, not a replacement for `make test`. Run it against the
pinned Sentry 24.7.1 and 26.10.1 installations. Keep only the 26.10.1 stand running after a
successful final run.

Never copy the generated API token, a DSN, request credentials, or protected controller state
into a manifest, terminal transcript, screenshot, or Git. Local input manifests belong under the
ignored `.cache/acceptance` directory.

## Start a Sentry profile

Use the lightweight profile for the legacy project and alert contract. Sentry 26 monitor
acceptance requires the feature-complete profile because Cron deletion is asynchronous and Uptime
checks require dedicated services.

```sh
make sentry-up SENTRY_VERSION=24.7.1 SENTRY_PROFILE=errors-only SENTRY_SKIP_PULL=true
make sentry-bootstrap SENTRY_VERSION=24.7.1 SENTRY_PROFILE=errors-only
make sentry-smoke SENTRY_VERSION=24.7.1 SENTRY_PROFILE=errors-only

make sentry-up SENTRY_VERSION=26.10.1 SENTRY_PROFILE=feature-complete SENTRY_SKIP_PULL=true
make sentry-bootstrap SENTRY_VERSION=26.10.1 SENTRY_PROFILE=feature-complete
make sentry-smoke SENTRY_VERSION=26.10.1 SENTRY_PROFILE=feature-complete
```

The manager refuses to run both pinned Sentry versions concurrently. Stop 24.7.1 before starting
26.10.1. Build and install the operator through the documented Make and Helm targets; do not edit
generated CRDs or RBAC in the cluster.

## Lifecycle matrix

Exercise these operations through Kubernetes for every applicable resource. Before retrying an
ambiguous create, list the remote Sentry objects in the browser or public API and prove that no
matching object already exists.

| Resource | Create | Update | Disable | Delete | Retain/no adoption | Browser evidence |
| --- | --- | --- | --- | --- | --- | --- |
| `SentryConnection` | install | refresh and compatibility validation | n/a | remove only after dependants | n/a | organization and integration context |
| `SentryProject` | required | required | `paused`, adoption, and connection fencing | `DeleteProject` | required | every managed project setting |
| `SentryAlertPolicy` | required through a consumer | required | every alert kind | after consumer removal | n/a | detector/workflow graph and actions |
| `SentryCronMonitor` | required | both schedule variants | required | required | required | details and edit forms |
| `SentryUptimeMonitor` | required | request and assertions | required | required | required | details, edit form, and successful Test Monitor |

For `SentryProject`, inspect General Settings, Project Teams, Alert Settings, Ownership Rules,
Inbound Filters, Security & Privacy, Issue Grouping, Debug Files, Client Keys, Loader Script, and
the source integration's Code Mappings tab. For each alert, inspect both the detector and connected
workflow. Sentry's automatically created Error Monitor is deliberately unmanaged and must remain
untouched.

Cron acceptance must verify schedule, timezone when using crontab, grace period, maximum runtime,
failure and recovery tolerances, owner, and enabled state. Application check-in emission remains
outside the controller. A successful and a failed check-in transition require a healthy Relay and
monitor ingestion pipeline.

Uptime acceptance must verify project, environment, URL, method, interval, timeout, header and body
values sourced from the fixed Secret, sampling, failure and recovery tolerances, owner, enabled
state, and all assertions. Use the UI's **Test Monitor** action and require a successful result.
The Sentry 26.10.1 UI does not expose `responseCaptureEnabled`; verify that field through the public
read API in addition to the UI.

For Application Metrics, run `make sentry-metrics-fixture` after the project is Ready. Require all
three metric names in the Sentry catalog, then create a bounded `application-metric` alert through
the CRD and inspect its selected metric, aggregate or equation, thresholds, owner, and connected
workflow in the detector UI. A dynamic alert on a stand without Seer must report
`DynamicDetectionUnavailable` while detector and workflow counts remain unchanged.

For a GitHub issue action, bootstrap a non-delivery source-code fixture and map it through
`SentryConnection.spec.integrations.sourceCode`. Verify the workflow UI shows the GitHub action,
the selected integration, and the Repository, Assignee, and Labels fields. Verify their exact saved
values through the workflow read API because the local fixture deliberately cannot perform live
GitHub autocomplete. Update one value, remove the action, prove it disappears from the API and UI,
restore it, and repeat the API/UI check after an operator restart. The feature-complete Sentry 26
profile enables issue-form prefetch so opening the form does not call GitHub merely to construct its
schema.

Run `make verify-upgrade` against the same Sentry 26 stand. It must preserve exactly one remote
project and one protected binding while the previous controller is stopped, the alpha
`SentryProject` object is migrated to the flat schema, and the CRD/chart is upgraded. It must then
survive a current-controller restart and remove the remote fixture through guarded deletion. A
binary rollback across this explicitly breaking alpha API migration is not supported.

## Restart and state recovery

After all resources are Ready, restart the operator three times. Wait for leader election and all
five controllers after each rollout. Then change one project setting, one alert field, and one
monitor field. Require the same protected remote identities, no duplicate remote objects, current
`status.observedGeneration`, and the changed values in the browser.

Inspect operator logs after the final mutation. Expected validation failures from deliberate
negative cases must use stable reasons and must not contain Secret values, DSNs, remote IDs, or raw
protected state.

## Completion record

A release candidate passes only when all of the following are true:

- all five CRDs report current observed generations and the applicable Ready conditions;
- create, update, disable, delete, Retain, three restarts, and post-restart mutation have passed;
- every public setting has browser evidence, with the one documented Uptime API-only exception;
- no duplicate, orphaned, or reassigned managed object exists;
- `make test`, `make lint`, `make verify-english`, and `git diff --check` pass;
- the 24.7.1 stand is stopped and exactly one working 26.10.1 stand remains.

