# Any local Postgres 18 whose user may CREATE DATABASE. With no user in the
# URL, the client connects as the current OS user (the Homebrew default).
BLYGD_TEST_DATABASE_URL ?= postgres://localhost:5432/postgres?sslmode=disable

.PHONY: build test test-all run vet

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
