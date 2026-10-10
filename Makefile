# Any local Postgres 18 whose user may CREATE DATABASE. With no user in the
# URL, the client connects as the current OS user (the Homebrew default).
BLYGD_TEST_DATABASE_URL ?= postgres://localhost:5432/postgres?sslmode=disable

.PHONY: build test test-all run vet parity interop

build:
	go build -o bin/blygd ./cmd/blygd

vet:
	go vet ./...

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
	BLYGD_REFERENCE_DIR='$(abspath $(REFERENCE))' go test -count=1 -run TestReferenceParity -v ./internal/blyg

# Live interop: both studios on localhost, each subscribed to the other.
# See scripts/interop/README.md.
interop:
	scripts/interop/run.sh '$(abspath $(REFERENCE))'
