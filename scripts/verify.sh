#!/bin/sh
set -eu
go test ./...
go vet ./...
go build ./cmd/lachesis
./scripts/scan-fixtures.sh
./scripts/offline-smoke.sh
