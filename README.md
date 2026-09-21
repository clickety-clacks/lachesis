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

Lachesis records normalized usage observations in `usage-history.json` under the
state directory. It keeps a bounded seven-day history after installation, with
no cache backfill. Read an account's samples at
`/api/v1/accounts/{id}/usage/history` and its 24-hour and 48-hour per-window
forecast at `/api/v1/accounts/{id}/usage/forecast`. Forecasts report coverage,
sample counts, reset boundaries, stale and reauthentication states, and leave
zero-burn or insufficient history without a finite exhaustion estimate.

While the service runs, it samples about every 15 minutes. Each successful fresh
usage read also records an observation. Forecast requests use the stored history
and never trigger a provider read. The default estimate uses up to 48 hours and
shows a 24-hour comparison; at least one hour of observed coverage is needed for
a useful burn estimate. Flat intervals at 100 percent are censored demand, so
rates use observed uncapped intervals and qualify the unknown demand while capped.
The aggregate forecast at `/api/v1/usage/forecast` keeps comparable provider,
plan, window, and duration pools separate and reports incomplete capacity when
an account has stale usage, missing history, or an unknown limit. Its pooled
estimate assumes switching work between accounts with the same known plan and
window capacity. Overlapping limits can strand capacity, so pooled runway is an
optimistic capacity estimate rather than a routing guarantee.
