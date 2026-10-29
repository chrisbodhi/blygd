#!/usr/bin/env bash
# The server half of scripts/deploy.sh, run as root:
#
#   upgrade.sh [--check] <staged binary> <sha256>
#
# Takes a fresh backup, restores it into a scratch database, and runs the
# new binary's migrations and a rebuild of every origin there, so a release
# meets real data before the live database does. With --check it stops
# there. Otherwise it stops blygd, migrates, installs the binary (keeping
# the old one as blygd.prev), starts it, checks /healthz and the running
# binary's checksum, and rebuilds every origin. When it fails before any
# migration ran, the old binary is put back.
set -euo pipefail

check=false
if [[ ${1:-} == --check ]]; then
	check=true
	shift
fi
[[ $# == 2 ]] || { echo "usage: upgrade.sh [--check] <binary> <sha256>" >&2; exit 2; }
staged=$1 want=$2
[[ $EUID == 0 ]] || { echo "run as root" >&2; exit 2; }
cd / # runuser keeps the working directory, which blygd and postgres may not read

ENV=/etc/blygd/blygd.env
BIN=/usr/local/bin/blygd
BACKUPS=/var/backups/blygd
SCRATCH=blygd_preflight
SCRATCH_URL="postgres:///$SCRATCH?host=/var/run/postgresql"

say() { echo "upgrade: $*"; }
die() { echo "upgrade: $*" >&2; exit 1; }

# blygd_env [VAR=value...] command... runs as blygd with its settings.
blygd_env() {
	# shellcheck disable=SC2016 # expanded by the inner shell
	runuser -u blygd -- sh -c 'set -a; . "$0"; set +a; exec env "$@"' "$ENV" "$@"
}

# The binary goes somewhere root owns before anything runs it.
install -d -m755 /usr/local/lib/blygd
new=/usr/local/lib/blygd/blygd-$want
install -m755 "$staged" "$new"
rm -f "$staged"
got=$(sha256sum "$new" | cut -d' ' -f1)
[[ $got == "$want" ]] || die "checksum $got, want $want"
blygd_env "$new" version

# migrate status: "database at migration N; this binary has M".
status() {
	blygd_env "$new" migrate status |
		sed -n 's/^database at migration \([0-9]*\); this binary has \([0-9]*\)$/\1 \2/p'
}
read -r current latest <<<"$(status)" || true
[[ -n ${current:-} && -n ${latest:-} ]] || die "could not read the migration status"
((current <= latest)) || die "the database is at migration $current, newer than this binary's $latest"
say "database at migration $current; release has $latest"

handles() { # handles [BLYGD_DATABASE_URL=...]
	# shellcheck disable=SC2016
	blygd_env "$@" sh -c 'psql "$BLYGD_DATABASE_URL" -XAtq -c "SELECT handle FROM users ORDER BY handle"'
}

# A fresh backup, offsite too when configured.
marker=$(mktemp)
trap 'rm -f "$marker"' EXIT
systemctl start blygd-backup
dump=$(find "$BACKUPS" -name 'blygd-*.dump' -newer "$marker" | sort | tail -1)
[[ -n $dump ]] || die "the backup wrote no new dump"
say "backed up to $dump"

# The release against a copy of that backup.
dropscratch() { runuser -u postgres -- env PGOPTIONS="-c client_min_messages=warning" dropdb --if-exists "$SCRATCH"; }
dropscratch
trap 'rm -f "$marker"; dropscratch' EXIT
runuser -u postgres -- createdb -O blygd "$SCRATCH"
runuser -u blygd -- pg_restore --no-owner --exit-on-error -d "$SCRATCH" "$dump"
blygd_env BLYGD_DATABASE_URL="$SCRATCH_URL" "$new" migrate
for h in $(handles BLYGD_DATABASE_URL="$SCRATCH_URL"); do
	blygd_env BLYGD_DATABASE_URL="$SCRATCH_URL" "$new" rebuild "$h"
done
say "preflight passed: migrated a copy and rebuilt $(handles BLYGD_DATABASE_URL="$SCRATCH_URL" | wc -l) origin(s)"
dropscratch
trap 'rm -f "$marker"' EXIT

if $check; then
	rm -f "$new"
	say "check only: nothing installed"
	exit 0
fi

# Stop, migrate, install, start. Migrating between stop and start keeps
# the old binary off the new schema, whatever BLYGD_MIGRATE_ON_START says.
systemctl stop blygd
if ((current < latest)) && ! blygd_env "$new" migrate; then
	read -r now _ <<<"$(status)" || true
	if [[ ${now:-} == "$current" ]]; then
		systemctl start blygd
		die "migration failed and changed nothing; the old binary is running again"
	fi
	die "migration failed at ${now:-an unknown migration}: blygd is stopped, and the old binary refuses this schema (DEPLOY.md, \"Rolling back\")"
fi
[[ -e $BIN ]] && cp -p "$BIN" "$BIN.prev"
install -m755 "$new" "$BIN"
systemctl start blygd

listen=$(sed -n 's/^BLYGD_LISTEN=//p' "$ENV" | tail -1)
listen=${listen:-:8080}
[[ $listen == :* ]] && listen=127.0.0.1$listen
healthy() {
	for _ in $(seq 60); do
		curl -fsS -o /dev/null "http://$listen/healthz" 2>/dev/null && return 0
		sleep 1
	done
	return 1
}
running() {
	local pid
	pid=$(systemctl show -p MainPID --value blygd)
	[[ $pid != 0 ]] && sha256sum "/proc/$pid/exe" | cut -d' ' -f1
}
if ! healthy || [[ $(running) != "$want" ]]; then
	journalctl -u blygd -n 20 --no-pager || true
	if ((current == latest)) && [[ -e $BIN.prev ]]; then
		install -m755 "$BIN.prev" "$BIN"
		systemctl restart blygd
		die "the new binary did not come up healthy; the old one is back"
	fi
	die "the new binary did not come up healthy after migrating: ship a fix forward (DEPLOY.md, \"Rolling back\")"
fi
rm -f "$new"
say "running $want"

for h in $(handles); do
	blygd_env "$BIN" rebuild "$h"
done
say "rebuilt every origin"
printf 'migrations %03d\n' "$latest"
