# Support

Sentry Kubernetes Operator is alpha software maintained on a best-effort basis. It is not
production-ready, has no support SLA, and currently makes no promise of backward-compatible
upgrades or external notification delivery.

## Self-service documentation

Start with:

- [`README.md`](README.md) for project scope and installation entry points;
- [`docs/quickstart.md`](docs/quickstart.md) for the minimal service-owner path;
- [`docs/configuration.md`](docs/configuration.md) for the complete CRD configuration guide;
- [`charts/sentry-kubernetes-operator/README.md`](charts/sentry-kubernetes-operator/README.md)
  for Helm installation and lifecycle;
- [`docs/troubleshooting.md`](docs/troubleshooting.md) for condition-driven diagnosis;
- [`docs/project-status.md`](docs/project-status.md) for implemented, deferred, and
  externally blocked work;
- [`docs/admin-recovery.txt`](docs/admin-recovery.txt) for read-only protected-state
  investigation.

## Where to ask

Use the GitHub issue forms for:

- reproducible operator or chart bugs;
- documentation problems;
- bounded feature proposals;
- usage questions that are not answered by the documentation.

Search existing issues first. Include exact operator commit or image digest, chart version,
Kubernetes version, Sentry version/profile, relevant redacted resource status, and the
smallest synthetic reproduction. State whether the behavior was observed with a real Sentry
installation or only with fixtures, envtest, or rendered configuration.

Do not use public issues for vulnerabilities, credentials, private DSNs, Secret data,
protected-state contents, customer events, or raw external response bodies. Follow
[`SECURITY.md`](SECURITY.md) for private reporting.

## What maintainers can reasonably help with

Maintainers can clarify the documented API, investigate reproducible defects, review scoped
proposals, and explain repository verification workflows. They may close requests that are
already documented, cannot be reproduced, omit required environment details, or fall outside
the current alpha boundary.

The project cannot provide operational support for third-party Sentry installations,
Kubernetes distributions, Gateway implementations, Prometheus installations, cloud
providers, registries, Slack, PagerDuty, Telegram, DNS, certificates, or provider account
configuration. Problems in those systems should be taken to the relevant vendor or project.

## Version policy

Until a stable release exists, support targets the current `main` branch and the latest
published prerelease only. Compatibility evidence is limited to the versions explicitly
listed in [`docs/compatibility.txt`](docs/compatibility.txt) and
[`docs/kubernetes-compatibility.txt`](docs/kubernetes-compatibility.txt).
