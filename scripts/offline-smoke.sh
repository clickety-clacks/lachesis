#!/bin/sh
set -eu
smoke_dir="$(mktemp -d)"
bin="$smoke_dir/lachesis"
pid=''
cleanup() {
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then kill "$pid"; wait "$pid" || true; fi
  rm -rf "$smoke_dir"
}
trap cleanup EXIT INT TERM
go build -o "$bin" ./cmd/lachesis
"$bin" serve --state-dir "$smoke_dir/state" >"$smoke_dir/stdout" 2>"$smoke_dir/stderr" &
pid=$!
i=0
until curl -fsS http://127.0.0.1:7843/api/v1/health >"$smoke_dir/health.json"; do
  i=$((i+1)); [ "$i" -lt 50 ] || { cat "$smoke_dir/stderr" >&2; exit 1; }; sleep 0.1
done
curl -fsS http://127.0.0.1:7843/api/v1/help >"$smoke_dir/help.json"
code="$(curl -sS -o "$smoke_dir/usage.json" -w '%{http_code}' http://127.0.0.1:7843/api/v1/usage)"
[ "$code" = 409 ]
grep -q 'NO_ACCOUNTS_ONBOARDED' "$smoke_dir/usage.json"
ss -ltn 2>/dev/null | grep -q '127.0.0.1:7843'
if ss -ltn 2>/dev/null | grep -Eq '0\.0\.0\.0:7843|\[::\]:7843'; then exit 1; fi
kill "$pid"; wait "$pid"; pid=''
