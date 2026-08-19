# Human-only eezo proof

Run these checks only with approved throwaway or designated accounts. Do not record credentials, account identifiers, email addresses, live usage figures, or raw responses.

1. Adopt four Codex stores and two Claude stores. Confirm one aggregate call returns six results.
2. Complete one API-driven browser onboarding without a file edit.
3. Prove Codex and Claude silent refresh preserves the original CLI store and CLI access.
4. Prove a running matching CLI causes `CREDENTIAL_STORE_BUSY` before a token call.
5. Interrupt after provider acceptance and before local file commit. Confirm the old credential still works.
6. Prove native Keychain update and create interruption yields complete old-or-new and absent-or-complete-new state.
7. Read labels and normalized headroom with provider-neutral JSON fields.

Any credential loss, disclosure, inference request, misleading headroom, failed CLI access, or failed Keychain atomic proof blocks release.
