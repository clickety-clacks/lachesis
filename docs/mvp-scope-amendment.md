# MVP file-store scope amendment

Mike approved this amendment on 2026-08-19. It supersedes the reviewed service spec only
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

Native Keychain update/create interruption proof is outside the MVP. The remaining
provider compatibility proof requires two independently approved sanitized-real usage
fixtures: one Codex fixture and one Claude fixture. Synthetic provider-shape tests,
malformed-response tests, the fixture sanitizer and scanner, and offline smoke remain
mandatory repository gates. Fixture absence stays truthful and does not certify provider
compatibility.

No human proof may record credentials, request headers, account identifiers, private
paths, raw response bodies, or live plan or usage values.
