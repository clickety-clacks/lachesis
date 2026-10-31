# Manual verification

Run these checks with disposable or designated test accounts. Do not record credentials,
account identifiers, email addresses, live usage figures, or raw responses.

1. Adopt four file-backed Codex stores and two file-backed Claude stores. Confirm one aggregate call returns six results.
2. Complete one API-driven browser onboarding without a file edit.
3. Prove Codex and Claude silent refresh preserves the original CLI store and CLI access.
4. Prove a running matching CLI causes `CREDENTIAL_STORE_BUSY` before a token call.
5. Interrupt after provider acceptance and before local file commit. Confirm the old credential still works.
6. Read labels and normalized headroom with provider-neutral JSON fields.

The current fixture status is Claude `available` and Codex `unavailable`; see
`testdata/evidence/provider-fixtures.json`. Fixture-dependent compatibility checks skip
when the corresponding fixture is unavailable. Real-response fixtures are useful
compatibility evidence. One maintainer may add a sanitized-real fixture when convenient; a
second-person approval is not required. An unavailable fixture is disclosed and does not
block release. Release notes should disclose when a provider has not been checked against
a real response.

Any credential loss, disclosure, inference request, misleading headroom, or failed CLI
access blocks release.
