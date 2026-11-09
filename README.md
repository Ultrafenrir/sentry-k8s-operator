# Sentry Kubernetes Operator

This operator manages Sentry projects and alert rules through Kubernetes custom resources.
Its public interface consists of three CRDs:

- `SentryConnection` defines the Sentry API endpoint, credentials, access policy, and optional integration catalogs.
- `SentryProject` creates or observes a Sentry project and manages its configuration.
- `SentryAlertPolicy` defines reusable alert rules consumed by projects.

The default Helm installation contains the CRDs, one controller deployment, HTTP metrics,
protected controller state, and one `SentryConnection`. It does not install project or alert
policy instances. It has no certificate-manager, admission server, or network-policy dependency.

## Install with Helm

Create a values file for your Sentry installation:

```yaml
image:
  repository: ultrafenrir/sentry-k8s-operator
  tag: v0.1.0
  pullPolicy: IfNotPresent

sentryConnection:
  name: company-sentry
  spec:
    url: https://sentry.example.com
    organization: company
    credentials:
      tokenSecretRef:
        name: sentry-api-token
        key: token
    compatibility:
      alerts: modern-26.10.1
    projectManagement:
      unmanagedProjects: Reject
    access:
      - owner: payments
        namespaces:
          matchNames: [payments]
        teams: [payments]
        allowCreate: true

controllerMonitoring:
  enabled: false
```

The token Secret is deliberately not stored in Helm values. Create it in the namespace selected
for this Helm release, then install the chart:

```sh
export OPERATOR_NAMESPACE=observability

kubectl create namespace "$OPERATOR_NAMESPACE"
kubectl -n "$OPERATOR_NAMESPACE" create secret generic sentry-api-token \
  --from-literal=token='YOUR_SENTRY_API_TOKEN'

helm upgrade --install sentry-operator ./charts/sentry-kubernetes-operator \
  --namespace "$OPERATOR_NAMESPACE" \
  --create-namespace \
  -f values-production.yaml
```

See [docs/quickstart.md](docs/quickstart.md) for test project and alert policy examples.

## Development

```sh
make help
make generate manifests
make test
make lint
```
