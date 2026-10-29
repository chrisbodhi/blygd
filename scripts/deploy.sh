#!/usr/bin/env bash
# Ship a commit to the blygd host (docs/DEPLOY.md, "Ship it"):
#
#   scripts/deploy.sh [--check] <ssh destination> [commit]
#
# Builds the commit (HEAD by default) from a clean clone, so the binary
# names it and nothing uncommitted rides along, then hands it to that
# commit's deploy/upgrade.sh on the host, under sudo. --check stops after
# the preflight: a fresh backup, restored into a scratch database, migrated
# and rebuilt there by the new binary. A real deploy adds its row to
# docs/DEPLOYS.md; write the notes and commit it.
set -euo pipefail

die() { echo "deploy: $*" >&2; exit 1; }

check=()
if [[ ${1:-} == --check ]]; then
	check=(--check)
	shift
fi
[[ $# == 1 || $# == 2 ]] || die "usage: scripts/deploy.sh [--check] <ssh destination> [commit]"
host=$1
root=$(git rev-parse --show-toplevel)
commit=$(git -C "$root" rev-parse --verify "${2:-HEAD}^{commit}")

case $(ssh "$host" uname -m) in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unknown architecture on $host" ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# A clone, not a worktree: Go does not stamp the commit into a build from
# a worktree outside the repository.
git clone --quiet --no-checkout "$root" "$work/src"
git -C "$work/src" checkout --quiet --detach "$commit"
out=$root/bin/blygd-linux
mkdir -p "$root/bin"
GOFLAGS='' GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -C "$work/src" -trimpath -o "$out" ./cmd/blygd

info=$(go version -m "$out")
grep -q "vcs.revision=$commit" <<<"$info" || die "the binary does not name $commit"
grep -q "vcs.modified=false" <<<"$info" || die "the build tree had changes"
gover=$(awk 'NR == 1 { print $2 }' <<<"$info")
xnet=$(awk '$1 == "dep" && $2 == "golang.org/x/net" { print $3 }' <<<"$info")
sum=$(shasum -a 256 "$out" | cut -d' ' -f1)
echo "deploy: built ${commit:0:7} for linux/$arch with $gover, sha256 $sum"

scp -q "$out" "$host:/tmp/blygd-$sum"
scp -q "$work/src/deploy/upgrade.sh" "$host:/tmp/blygd-upgrade-$sum.sh"
log=$work/upgrade.log
ssh -t "$host" "sudo bash /tmp/blygd-upgrade-$sum.sh ${check[*]} /tmp/blygd-$sum $sum; s=\$?; rm -f /tmp/blygd-upgrade-$sum.sh /tmp/blygd-$sum; exit \$s" |
	tee "$log"

((${#check[@]} == 0)) || exit 0

migrations=$(tr -d '\r' <"$log" | sed -n 's/^migrations \([0-9]*\)$/\1/p')
[[ -n $migrations ]] || die "deployed, but the upgrade did not report its migrations: add the DEPLOYS.md row by hand"
row="| $(date -u +%F) | \`${commit:0:7}\` | $gover / $xnet | \`${sum:0:16}\` | $migrations | |"
awk -v row="$row" '{ print } !done && /^\|---/ { print row; done = 1 }' "$root/docs/DEPLOYS.md" >"$work/DEPLOYS.md"
cp "$work/DEPLOYS.md" "$root/docs/DEPLOYS.md"
echo "deploy: added to docs/DEPLOYS.md; write its notes and commit it"
