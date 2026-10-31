# MVP file-store scope amendment

This amendment was approved on 2026-08-19. It supersedes the reviewed service spec only
for Keychain-backed MVP bindings and native Keychain interruption proof.

## Binding scope

The MVP registers file-backed accounts only:

- A Codex home selected with `CODEX_HOME` stores its credential in `auth.json`.
- A Claude home selected with `CLAUDE_CONFIG_DIR` stores its credential in
  `.credentials.json`.
- New onboarding allocates one private provider home per account and invokes the matching
  CLI with that home override.
- Direct adoption accepts the provider default only when it safely resolves to a file
  home. Codex default adoption remains available. Claude default adoption requires an
  absolute `CLAUDE_CONFIG_DIR`; it never falls back to the personal login Keychain.
- An explicit `source.kind` of `keychain` returns `KEYCHAIN_SOURCE_UNSUPPORTED` with exact
  calls for new onboarding and explicit-home adoption.

The native Keychain adapter remains fail-closed and unreachable from the production store
factory. Native Keychain writes are not implemented.

## Release proof

Native Keychain update/create interruption proof is outside the MVP. Sanitized-real
provider fixtures are useful evidence about real response shapes, but their availability
is disclosure, not a release condition. The current status in
`testdata/evidence/provider-fixtures.json` is Claude `available` and Codex `unavailable`.
Fixture-dependent compatibility checks skip when their provider's fixture is unavailable.
Synthetic provider-shape tests, malformed-response tests, the fixture sanitizer and
scanner, and offline smoke remain mandatory repository gates. An unavailable or skipped
fixture does not certify provider compatibility. Release notes should disclose when a
provider has not been checked against a real response. A maintainer may add sanitized-real
evidence later when convenient; it does not need a second-person approval.

No human proof may record credentials, request headers, account identifiers, private
paths, raw response bodies, or live plan or usage values.
