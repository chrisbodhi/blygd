#!/usr/bin/env bash
# Nightly Postgres backup for blygd. Run as the postgres-peer user (blygd) via
# blygd-backup.timer. Keeps KEEP local dumps; if RCLONE_REMOTE is set, copies
# each new dump offsite (e.g. RCLONE_REMOTE=r2:blygd-backups). If
# BLYGD_BACKUP_PING_URL is set, it is fetched once everything above has
# succeeded, so a dead-man's-switch monitor (healthchecks.io and the like)
# notices a night with no ping. The URL is the only thing sent.
set -euo pipefail

DB="${BLYGD_BACKUP_DB:-blygd}"
DIR="${BLYGD_BACKUP_DIR:-/var/backups/blygd}"
KEEP="${BLYGD_BACKUP_KEEP:-7}"
RCLONE_REMOTE="${RCLONE_REMOTE:-}"
PING_URL="${BLYGD_BACKUP_PING_URL:-}"

umask 077
mkdir -p "$DIR"

out="$DIR/$DB-$(date -u +%Y%m%dT%H%M%SZ).dump"
tmp="$out.partial"
trap 'rm -f "$tmp"' EXIT

# Custom format: compressed, and pg_restore can restore selectively.
pg_dump --format=custom --no-owner --dbname="$DB" --file="$tmp"

# A dump that cannot be listed is not a backup.
pg_restore --list "$tmp" >/dev/null

mv "$tmp" "$out"
echo "wrote $out ($(du -h "$out" | cut -f1))"

if [[ -n "$RCLONE_REMOTE" ]]; then
	# The offsite key is upload-only (no listFiles), so skip the destination
	# check; each dump's name is unique, so nothing is overwritten.
	rclone copyto --no-check-dest "$out" "$RCLONE_REMOTE/$(basename "$out")"
	echo "uploaded to $RCLONE_REMOTE"
fi

# Prune local copies, newest KEEP stay. (Prune offsite with a bucket lifecycle rule.)
ls -1t "$DIR"/"$DB"-*.dump | tail -n +"$((KEEP + 1))" | xargs -r rm -f --

# Last, so a ping means the dump was written, listed, sent offsite and
# pruned. A failed ping does not fail the backup: the dump is good, and the
# missing ping is itself what the monitor reports.
if [[ -n "$PING_URL" ]]; then
	if curl -fsS --max-time 10 --retry 3 --output /dev/null "$PING_URL"; then
		echo "pinged the monitor"
	else
		echo "could not ping the monitor" >&2
	fi
fi
