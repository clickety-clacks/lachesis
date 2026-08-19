#!/bin/sh
set -eu
verify_dir="$(mktemp -d)"
cleanup() { rm -rf "$verify_dir"; }
trap cleanup EXIT INT TERM
go test ./...
go vet ./...
go build -o "$verify_dir/lachesis" ./cmd/lachesis
./scripts/scan-fixtures.sh
./scripts/offline-smoke.sh
