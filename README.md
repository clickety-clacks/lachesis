# Lachesis

Lachesis is a loopback-only JSON service for current usage across multiple Codex and Claude subscription accounts. Provider CLIs remain the credential authority.

Build and verify:

```sh
./scripts/verify.sh
```

Run:

```sh
./lachesis serve --state-dir /absolute/private/state
```

The service listens only on `127.0.0.1:7843`. Start with `GET /api/v1/help`.
