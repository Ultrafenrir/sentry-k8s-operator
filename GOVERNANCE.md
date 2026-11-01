# Governance

Sentry Kubernetes Operator currently uses a maintainer-led governance model. The project is
small and alpha; this document makes decision and release authority explicit without
implying a foundation, company, or vendor affiliation.

## Roles

### Contributors

Anyone who submits issues, documentation, tests, designs, reviews, or code under the project
license is a contributor. Contributors are expected to follow the Code of Conduct and the
repository's contribution and security policies.

### Maintainers

Maintainers have sustained responsibility for project direction and safety. They may merge
or close pull requests, triage issues, moderate community spaces, manage releases and
security reports, and update repository policy. Current maintainers are listed in
[`MAINTAINERS.md`](MAINTAINERS.md).

## Decision making

The project prefers public, evidence-based discussion in issues and pull requests. Decisions
should consider API stability, security boundaries, recoverability, cross-version behavior,
maintenance cost, and the documented alpha scope.

Maintainers seek rough consensus for ordinary changes. When consensus is not reached, the
lead maintainer makes the final decision and should record the reasoning in the relevant
issue, pull request, ADR, or project-status document. Security embargoes and private conduct
reports are handled privately until disclosure is safe.

Changes to public APIs, ownership or recovery semantics, supported-version policy, security
boundaries, or governance should receive explicit maintainer approval. Significant design
decisions should be recorded as an ADR under `docs/adr/`.

## Becoming a maintainer

A contributor may be invited to become a maintainer after demonstrating sustained,
high-quality contributions, sound judgment around security and compatibility, constructive
review, reliable follow-through, and adherence to the Code of Conduct. Existing maintainers
approve additions and update `MAINTAINERS.md` in a pull request.

Maintainers may step down at any time. A maintainer who is inactive for an extended period
may be moved to emeritus status after a good-faith attempt to contact them. Access should be
removed promptly when required for security or conduct reasons.

## Releases

Only maintainers may create release tags, publish official artifacts, or change signing and
release identities. A release must follow the repository's supply-chain policy, use an
immutable versioned image identity, update release notes or `CHANGELOG.md`, and state exact
verification and evidence boundaries.

The existence of a chart version or local package does not by itself mean that an official
hosted release has been published.

## Conflicts of interest

Maintainers should disclose material conflicts of interest and recuse themselves when a
decision directly affects their employer, client, or a report about their own conduct. The
remaining maintainers decide the path forward; if no unconflicted maintainer is available,
the project uses GitHub's platform reporting and moderation mechanisms.

## Amendments

Governance changes are proposed and reviewed through a pull request. The change should
explain why the current process is insufficient and how contributor rights, project safety,
and decision accountability are affected.
