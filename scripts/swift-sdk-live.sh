#!/usr/bin/env bash
# The Swift SDK against a real blygd: builds and serves this checkout on
# localhost over a fresh database, then runs the SDK's tests with
# LiveTests enabled (sdk/swift/Tests/BlygdTests/LiveTests.swift).
#
#   scripts/swift-sdk-live.sh        (or: make sdk-test-live)
#
# Needs: Swift 6, go, curl, and a Postgres 18 whose user may CREATE
# DATABASE (PSQL and BLYGD_PG_BASE override the client and server, as for
# make interop). Uses port 18091.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PSQL="${PSQL:-$(command -v psql || command -v psql-18 || echo /opt/homebrew/bin/psql-18)}"
PG_BASE="${BLYGD_PG_BASE:-postgres://localhost:5432}"
DB="blygd_swift_sdk_$$"
WORK="$(mktemp -d)"
PORT=18091

pid=""
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  wait 2>/dev/null || true
  "$PSQL" -q "$PG_BASE/postgres" -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

"$PSQL" -q "$PG_BASE/postgres" -c "CREATE DATABASE $DB"
(cd "$ROOT" && go build -o "$WORK/blygd" ./cmd/blygd)

export BLYGD_DATABASE_URL="$PG_BASE/$DB?sslmode=disable"
export BLYGD_PUBLIC_URL="http://127.0.0.1:$PORT/"
export BLYGD_LISTEN="127.0.0.1:$PORT"
export BLYGD_ALLOW_PRIVATE_FETCH=true  # the test subscribes to its own origin on localhost
export BLYGD_MANUAL_POLL_FLOOR=1       # development only: the test polls twice
"$WORK/blygd" migrate >/dev/null
ADDED="$("$WORK/blygd" user add ada "Ada")"
TOKEN="$(printf '%s\n' "$ADDED" | sed -n 's/^owner token (shown once): //p')"
"$WORK/blygd" serve >"$WORK/blygd.log" 2>&1 &
pid=$!

for _ in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/instance")" = 200 ] && break
  sleep 0.5
done

status=0
(cd "$ROOT" && BLYGD_SDK_TEST_URL="http://127.0.0.1:$PORT" BLYGD_SDK_TEST_TOKEN="$TOKEN" swift test) || status=$?
if [ "$status" -ne 0 ]; then
  echo; echo "--- blygd log (tail)"; tail -20 "$WORK/blygd.log"
fi
exit "$status"
