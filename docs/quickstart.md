# Quickstart

The chart installs a `SentryConnection`, but applications own their `SentryProject` and
`SentryAlertPolicy` resources and should ship them in their own charts.

The connection access rule must allow the project namespace, owner, and Sentry team. An alert
also requires an installed Slack or PagerDuty integration declared by the connection and granted
by the access rule. This example uses Slack:

```yaml
apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryAlertPolicy
metadata:
  name: backend-alerts
spec:
  sentry: company-sentry
  notificationTargets:
    slack:
      company-slack:
        - backend-alerts
  alerts:
    new-errors:
      trigger: event-count
      windowMinutes: 5
      threshold: 1
      notify:
        - slack/backend-alerts
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
