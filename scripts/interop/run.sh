#!/usr/bin/env bash
# Live interop check: blygd and the reference client (blygger-studio) on
# localhost, each subscribed to the other. See scripts/interop/README.md.
#
#   scripts/interop/run.sh path/to/blygger-studio
#
# Needs: a built reference checkout (npm ci && npm run build), node, python3,
# go, and a Postgres 18 whose user may CREATE DATABASE (PSQL and
# BLYGD_PG_BASE override the client and server). Uses ports 18090 (blygd),
# 18787 and 18788 (the reference and its cron control), and 18789 and 18790
# (two static relocated blygs that check.py serves itself).
set -euo pipefail

REF_DIR="$(cd "${1:?usage: run.sh path/to/blygger-studio}" && pwd)"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
PSQL="${PSQL:-$(command -v psql || command -v psql-18 || echo /opt/homebrew/bin/psql-18)}"
PG_BASE="${BLYGD_PG_BASE:-postgres://localhost:5432}"
DB="blygd_interop_$$"
WORK="$(mktemp -d)"
BLYGD_PORT=18090 REF_PORT=18787 REF_CONTROL_PORT=18788
export REF_PORT REF_CONTROL_PORT
export REF_PASSWORD="interop-password"

"$ROOT/scripts/check-node.sh"
[ -d "$REF_DIR/node_modules" ] || { echo "build the reference first: (cd $REF_DIR && npm ci && npm run build)"; exit 2; }

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  "$PSQL" -q "$PG_BASE/postgres" -c "DROP DATABASE IF EXISTS $DB" >/dev/null 2>&1 || true
  # INTEROP_LOGS=dir keeps both studios' full logs.
  [ -n "${INTEROP_LOGS:-}" ] && mkdir -p "$INTEROP_LOGS" && cp "$WORK"/*.log "$INTEROP_LOGS"/ 2>/dev/null
  rm -rf "$REF_DIR/.blygd-interop" "$WORK"
}
trap cleanup EXIT

wait_for() { # url: until it answers 200 (Miniflare listens before its migrations finish)
  for _ in $(seq 1 120); do [ "$(curl -s -o /dev/null -w '%{http_code}' "$1")" = 200 ] && return 0; sleep 0.5; done
  echo "timed out waiting for $1"; return 1
}

echo "reference: $(git -C "$REF_DIR" log -1 --format='%h %cd' 2>/dev/null || echo unknown)"
"$PSQL" -q "$PG_BASE/postgres" -c "CREATE DATABASE $DB"
(cd "$ROOT" && go build -o "$WORK/blygd" ./cmd/blygd)

export BLYGD_DATABASE_URL="$PG_BASE/$DB?sslmode=disable"
export BLYGD_PUBLIC_URL="http://127.0.0.1:$BLYGD_PORT/"
export BLYGD_LISTEN="127.0.0.1:$BLYGD_PORT"
export BLYGD_ALLOW_PRIVATE_FETCH=true   # both studios are on localhost
export BLYGD_MANUAL_POLL_FLOOR=2        # development only: keep the run short
export BLYGD_RATE_LIMITS=false          # the checks run faster than any budget allows
TOKEN="$("$WORK/blygd" user add alice "Alice on blygd" | sed -n 's/^owner token (shown once): //p')"
"$WORK/blygd" serve >"$WORK/blygd.log" 2>&1 &
pids+=($!)

mkdir -p "$REF_DIR/.blygd-interop"
cp "$HERE/reference-server.mts" "$REF_DIR/.blygd-interop/server.mts"
(cd "$REF_DIR" && exec node --import tsx .blygd-interop/server.mts) >"$WORK/reference.log" 2>&1 &
pids+=($!)

wait_for "http://127.0.0.1:$BLYGD_PORT/alice/blyg.json"
wait_for "http://127.0.0.1:$REF_PORT/blyg.json"

status=0
BLYGD_URL="http://127.0.0.1:$BLYGD_PORT" BLYGD_ORIGIN="http://127.0.0.1:$BLYGD_PORT/alice/" BLYGD_TOKEN="$TOKEN" \
REF_URL="http://127.0.0.1:$REF_PORT/" REF_CONTROL="http://127.0.0.1:$REF_CONTROL_PORT/" \
  python3 "$HERE/check.py" || status=$?
if [ "$status" -ne 0 ]; then
  echo; echo "--- blygd log (tail)"; tail -20 "$WORK/blygd.log"
  echo "--- reference log (tail)"; tail -20 "$WORK/reference.log"
fi
exit "$status"
