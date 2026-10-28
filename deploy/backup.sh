#!/usr/bin/env bash
# Nightly Postgres backup for blygd. Run as the postgres-peer user (blygd) via
# blygd-backup.timer. Keeps KEEP local dumps; if RCLONE_REMOTE is set, copies
# each new dump offsite (e.g. RCLONE_REMOTE=r2:blygd-backups).
set -euo pipefail

DB="${BLYGD_BACKUP_DB:-blygd}"
DIR="${BLYGD_BACKUP_DIR:-/var/backups/blygd}"
KEEP="${BLYGD_BACKUP_KEEP:-7}"
RCLONE_REMOTE="${RCLONE_REMOTE:-}"

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
