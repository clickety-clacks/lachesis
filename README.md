# Lachesis

Lachesis is a loopback-only JSON service for current usage across multiple Codex and Claude subscription accounts. Provider CLIs remain the credential authority.

The MVP registers file-backed provider homes only. Codex homes use `auth.json`. Claude
homes selected through `CLAUDE_CONFIG_DIR` use `.credentials.json`. New onboarding creates
an isolated per-account provider home and keeps the matching CLI as credential authority.
Keychain account sources are outside the MVP and return a structural remedy for onboarding
or adopting an explicit file home. See [the MVP scope amendment](docs/mvp-scope-amendment.md).

Build and verify:

```sh
./scripts/verify.sh
```

Run:

```sh
./lachesis serve --state-dir /absolute/private/state
```

The service listens only on `127.0.0.1:7843`. Start with `GET /api/v1/help`.
