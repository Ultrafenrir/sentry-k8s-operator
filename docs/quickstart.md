# Quickstart

The chart installs a `SentryConnection`, but applications own their `SentryProject` and
`SentryAlertPolicy` resources and should ship them in their own charts.

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
  project:
    create:
      team: payments
      platform: python
      displayName: Checkout
  grantAccess:
    teams:
      - backend
  clientKeys:
    browser:
      active: true
      rotationRevision: 1
  dsn:
    managedClientKey: browser
  lifecycle:
    onDelete: Retain
```

Apply the resources only after the connection reports `Ready=True`:

```sh
kubectl get sentryconnection company-sentry
kubectl -n payments create secret generic sentry-dsns
kubectl apply -f policy-and-project.yaml
kubectl -n payments get sentryproject checkout -o yaml
kubectl -n payments get secret sentry-dsns -o jsonpath='{.data.checkout}' | base64 --decode
```

The project becomes ready only after the connection, project, DSN, managed client key, and alert
components have all been observed back from Sentry. See
[configuration.md](configuration.md) for every supported field, compatibility rules, and status
reasons.
