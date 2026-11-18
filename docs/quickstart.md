# Quickstart

The chart installs a `SentryConnection`, but applications own their `SentryProject` and
`SentryAlertPolicy`, `SentryCronMonitor`, and `SentryUptimeMonitor` resources and should ship them
in their own charts.

Kubernetes RBAC controls who may create a namespaced project. The primary team and any additional
team access are declared on the project. The example below uses only team email, so it does not
require an external notification integration:

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryAlertPolicy
metadata:
  name: backend-alerts
spec:
  sentry: company-sentry
  alerts:
    new-errors:
      enabled: true
      description: New production errors
      ownerTeam: backend
      issue:
        triggerMatch: any
        triggers:
          - type: first-seen
          - type: regression
        filterMatch: all
        filters:
          - type: level
            operator: gte
            value: error
      actions:
        - status: triggered
          email:
            target: team
            team: backend
---
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: checkout
  namespace: payments
spec:
  sentry: company-sentry
  alertPolicy: backend-alerts
  team: payments
  platform: python
  displayName: Checkout
  grantAccess:
    teams:
      - backend
  lifecycle:
    onDelete: Retain
---
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryCronMonitor
metadata:
  name: checkout-settlement
  namespace: payments
spec:
  projectRef: checkout
  interval:
    value: 15
    unit: minute
  checkInMarginMinutes: 5
  maxRuntimeMinutes: 20
  failureThreshold: 2
  recoveryThreshold: 1
  enabled: true
  onDelete: Delete
```

Apply the resources only after the connection reports `Ready=True`:

```sh
kubectl get sentryconnection company-sentry
kubectl -n payments create secret generic sentry-dsns
kubectl apply -f policy-and-project.yaml
kubectl -n payments get sentryproject checkout -o yaml
kubectl -n payments get secret sentry-dsns -o jsonpath='{.data.checkout}' | base64 --decode
```

The project becomes ready only after the connection, project, automatically selected public DSN,
and alert components have all been observed back from Sentry. A newly created Sentry project has
one active default client key, so the operator publishes that key without additional configuration.
The Cron Monitor becomes ready separately;
application code is responsible for emitting its check-ins. See
[configuration.md](configuration.md) for every supported field, compatibility rules, and status
reasons.
