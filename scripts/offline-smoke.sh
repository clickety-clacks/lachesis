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
"$bin" serve --state-dir "$smoke_dir/SMOKE_SECRET_SENTINEL-state" >"$smoke_dir/stdout" 2>"$smoke_dir/stderr" &
pid=$!
i=0
until curl -fsS http://127.0.0.1:7843/api/v1/health >/dev/null; do
  i=$((i+1)); [ "$i" -lt 50 ] || { cat "$smoke_dir/stderr" >&2; exit 1; }; sleep 0.1
done
go run ./scripts/smokecheck.go http://127.0.0.1:7843
listeners="$(lsof -nP -a -p "$pid" -iTCP -sTCP:LISTEN -Fn | sed -n 's/^n//p')"
[ "$(printf '%s\n' "$listeners" | sed '/^$/d' | wc -l | tr -d ' ')" = 1 ]
[ "$listeners" = "127.0.0.1:7843" ]
if grep -Eiq 'SMOKE_SECRET_SENTINEL|access_token|refresh_token|id_token|authorization|cookie' "$smoke_dir/stdout" "$smoke_dir/stderr"; then exit 1; fi
kill "$pid"; wait "$pid"; pid=''
