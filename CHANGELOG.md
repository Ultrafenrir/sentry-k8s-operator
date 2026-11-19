# Changelog

## Unreleased

- Renamed the shared DSN output Secret to `sentry-dsn` and made the operator create it on first
  publication instead of requiring application namespaces to pre-create it.
- Reduced the distribution to one operator image and the three CRD interfaces.
- Made `SentryConnection` part of the default Helm installation.
- Simplified the release workflow to publish one Docker Hub image.
- Removed `SentryConnection.spec.access`; Kubernetes RBAC now controls access to namespaced
  `SentryProject` resources, and primary/additional Sentry teams are selected by each project.
- Moved cross-field CR validation into the controller so the CRDs remain installable without CEL
  admission support.
