#!/usr/bin/env bash
# Refuse a Node without the Brotli decoder fix (CVE-2026-111497,
# "Broccoli2Shell"): 22.23.4, 24.22.1 and 26.12.1 are the first patched
# releases of each line. parity and interop run the reference on it.
set -euo pipefail

v=$(node -p process.versions.node 2>/dev/null) || { echo "node not found" >&2; exit 2; }
IFS=. read -r major minor patch <<<"$v"
case $major in
22) want=22.23.4 ;;
24) want=24.22.1 ;;
26) want=26.12.1 ;;
*)
	((major > 26)) && exit 0
	echo "node $v is not a supported line: use 26.12.1 or later (or 24.22.1, 22.23.4)" >&2
	exit 1
	;;
esac
IFS=. read -r _ wminor wpatch <<<"$want"
if ((minor < wminor || (minor == wminor && patch < wpatch))); then
	echo "node $v lacks the Brotli fix (CVE-2026-111497): upgrade to $want or later" >&2
	exit 1
fi
