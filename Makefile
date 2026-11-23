# Any local Postgres 18 whose user may CREATE DATABASE. With no user in the
# URL, the client connects as the current OS user (the Homebrew default).
BLYGD_TEST_DATABASE_URL ?= postgres://localhost:5432/postgres?sslmode=disable

.PHONY: build test test-all run vet vuln parity interop sdk sdk-check sdk-test sdk-test-live

build:
	go build -o bin/blygd ./cmd/blygd

vet:
	go vet ./...

# Known vulnerabilities in the code paths blygd calls (needs the network).
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

test:
	go test ./...

test-all:
	BLYGD_TEST_DATABASE_URL='$(BLYGD_TEST_DATABASE_URL)' go test -count=1 ./...

run:
	go run ./cmd/blygd serve

# Differential tests against the reference client: same inputs through its
# TypeScript and our Go. Needs a built blygger-studio checkout
# (npm ci && npm run build): make parity REFERENCE=../blygger-studio
parity:
	scripts/check-node.sh
	BLYGD_REFERENCE_DIR='$(abspath $(REFERENCE))' go test -count=1 -run TestReferenceParity -v ./internal/blyg

# Live interop: both studios on localhost, each subscribed to the other.
# See scripts/interop/README.md.
interop:
	scripts/interop/run.sh '$(abspath $(REFERENCE))'

# The Swift SDK (sdk/swift/README.md). Needs a Swift 6 toolchain.
# Regenerate it after any change to internal/web/openapi.json: make test
# fails until you do (TestSwiftSDKIsCurrent).
sdk:
	scripts/swift-sdk.sh

# The committed client is exactly what the document generates.
sdk-check:
	scripts/swift-sdk.sh check

sdk-test: sdk-check
	swift test

# The SDK against this checkout served on localhost (as for interop, a
# Postgres 18 whose user may CREATE DATABASE; BLYGD_PG_BASE overrides it).
sdk-test-live: sdk-check
	scripts/swift-sdk-live.sh
