#!/bin/sh
# Generates the Swift SDK's client from internal/web/openapi.json.
#
#   scripts/swift-sdk.sh          regenerate sdk/swift/Sources/Blygd/Generated
#   scripts/swift-sdk.sh check    fail if the committed code is not what the
#                                 generator makes from the current document
#
# Needs a Swift 6 toolchain on PATH (Xcode 16+, or swift.org/install). The
# generator's version is pinned in sdk/swift/Generator/Package.swift.
#
# Each generated file records the SHA-256 of the document it came from, and
# TestSwiftSDKIsCurrent (internal/web/sdk_test.go) fails when that is not
# the document's hash: changing openapi.json means running this too.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
doc=internal/web/openapi.json
sdk=sdk/swift
generated=$sdk/Sources/Blygd/Generated

if ! command -v swift >/dev/null 2>&1; then
	echo "swift-sdk: no swift on PATH; install Xcode 16+ or a toolchain from swift.org/install" >&2
	exit 1
fi

cd "$root"
sum=$(shasum -a 256 "$doc" | cut -d' ' -f1)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The generator reads a copy in which the schemas the SDK replaces with
# hand-written types are emptied (sdk/swift/gendoc says why).
go run ./sdk/swift/gendoc "$doc" "$work/openapi.json"

generate() { # <output directory>
	swift run --quiet --package-path "$sdk/Generator" swift-openapi-generator generate \
		--config "$sdk/openapi-generator-config.yaml" \
		--additional-file-comment "From $doc (sha256 $sum). Regenerate with: make sdk" \
		--output-directory "$1" \
		"$work/openapi.json" >"$work/log" 2>&1 || { cat "$work/log" >&2; exit 1; }
	# The generator skips what it cannot model and carries on, so a schema
	# it does not support would vanish from the SDK with only a warning.
	if grep 'is not supported' "$work/log" >&2; then
		echo "swift-sdk: the generator dropped the schemas above; reshape them, or override them (sdk/swift/gendoc)" >&2
		exit 1
	fi
}

case "${1:-generate}" in
generate)
	rm -rf "$generated"
	mkdir -p "$generated"
	generate "$generated"
	;;
check)
	mkdir "$work/out"
	generate "$work/out"
	if ! diff -r "$generated" "$work/out" >/dev/null; then
		echo "swift-sdk: $generated is not what $doc generates; run make sdk" >&2
		diff -ru "$generated" "$work/out" | head -40 >&2
		exit 1
	fi
	;;
*)
	echo "usage: $0 [generate|check]" >&2
	exit 2
	;;
esac
