.PHONY: test vet build scan smoke verify
test:
	go test ./...
vet:
	go vet ./...
build:
	go build ./cmd/lachesis
scan:
	./scripts/scan-fixtures.sh
smoke:
	./scripts/offline-smoke.sh
verify:
	./scripts/verify.sh
