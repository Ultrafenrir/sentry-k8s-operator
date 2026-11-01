# Security policy

## Supported versions

This project is alpha and has not published a stable release. Security fixes are developed
only for the current `main` branch and, after releases begin, for the latest published
version. Older commits, prereleases, and locally modified builds are not supported.

| Version | Supported |
| --- | --- |
| Current `main` branch | Best-effort security fixes |
| Latest published release | Best-effort security fixes |
| Older versions and prereleases | No |

There is no security response SLA. The maintainers will make a good-faith effort to
acknowledge and assess actionable reports.

## Reporting a vulnerability

Do not report suspected vulnerabilities in public issues, discussions, pull requests, or
logs.

Use [GitHub private vulnerability reporting](https://github.com/Ultrafenrir/sentry-k8s-operator/security/advisories/new)
when it is available. If the private reporting form is unavailable, contact the repository
owner through a private method listed on the
[@Ultrafenrir GitHub profile](https://github.com/Ultrafenrir). Do not include production
tokens, DSNs, Secret contents, protected-state records, or customer data in the initial
message.

Include, when possible:

- the affected commit, image digest, chart version, and Kubernetes/Sentry versions;
- the component and configuration involved;
- a minimal reproduction using synthetic data;
- the security impact and required attacker access;
- whether the issue is already public or actively exploited;
- any suggested mitigation.

The maintainers may request additional information, coordinate a fix and disclosure date,
and credit the reporter if requested. Please allow time for a fix before public disclosure.

## Sensitive data and test reports

Never submit real API tokens, DSNs, bot tokens, integration keys, passwords, raw Kubernetes
Secret data, protected-state contents, or unsanitized Sentry payloads. Use reserved example
domains, synthetic identifiers, and the repository's sanitized fixture conventions.

If a credential is exposed, revoke or rotate it immediately. Removing it from Git history
does not make it safe to reuse.

## Security scope

Useful reports include authentication or authorization bypass, cross-namespace access,
secret disclosure, unsafe ownership takeover, untrusted payload execution, signature or
webhook validation bypass, and supply-chain policy bypass.

General hardening suggestions, support questions, missing external provider features, and
expected alpha limitations belong in the normal issue tracker after checking
[`SUPPORT.md`](SUPPORT.md). The project's current security and evidence boundaries are
documented in [`docs/project-status.md`](docs/project-status.md) and
[`docs/supply-chain.txt`](docs/supply-chain.txt).

## Safe-harbor intent

Good-faith research that respects user privacy, avoids service disruption, uses only systems
and data the researcher is authorized to access, and follows this policy is welcome. This
statement does not authorize testing against third-party Sentry, Telegram, Slack,
PagerDuty, cloud-provider, registry, or Kubernetes infrastructure.
