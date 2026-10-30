These fixtures are synthetic, sanitized and derived from version-pinned source contracts.
They are not recordings of a real Sentry instance and contain no production credentials,
events, recipients or external identities. Placeholder-looking values are fixture-only.

Capability status from these files is at most ContractTested, never Verified.

sentry-24.7.1 contains the legacy project/settings/rules/service-hook contracts.
sentry-26.10.1 contains public project-create/client-key/typed-PII/filter contracts,
modern workflow enable/disable and private service-hook source contracts. The workflow mutation is modeled as PUT
/api/0/organizations/{org}/workflows/?id={workflow_id}; IDs are explicit query selectors
and placeholders here. The service-hook fixture proves payload signing only; it does not
prove public endpoint access, feature enablement, TLS verification or delivery.
