#!/bin/bash
#
# Drive scripts/check-image-pins.sh through each outcome against the stand-in
# docker in scripts/testdata/check-image-pins, with no registry and no Docker
# daemon. Each case writes the registry's answers for that case and a compose
# file, then checks the script's exit code.
#
# Usage: scripts/check-image-pins_test.sh

set -euo pipefail

scripts="$(cd "$(dirname "$0")" && pwd)"
script="$scripts/check-image-pins.sh"
standin="$scripts/testdata/check-image-pins"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/fake"
export FAKE_DIR="$work/fake"

digest() { printf 'sha256:%064d' "$1"; }
D1=$(digest 1)
D2=$(digest 2)
D3=$(digest 3)

failures=0
# expect NAME WANT_EXIT COMPOSE_CONTENT: run the check on a fresh compose file.
expect() {
	local name="$1" want="$2"
	printf '%s' "$3" >"$work/compose.yml"
	local got=0
	PATH="$standin:$PATH" "$script" "$work/compose.yml" >"$work/out" 2>&1 || got=$?
	if [ "$got" -ne "$want" ]; then
		echo "FAIL $name: exit $got, want $want" >&2
		sed 's/^/    /' "$work/out" >&2
		failures=$((failures + 1))
	else
		echo "ok   $name"
	fi
}
reset() {
	rm -f "$FAKE_DIR"/*
	echo "$D1" >"$FAKE_DIR/acme_a_1.0"
	echo "$D3 $D2" >"$FAKE_DIR/acme_b_2.0"
}
two_pins="services:
  a:
    image: acme/a:1.0@$D1
  b:
    image: \"acme/b:2.0@$D2\"
"

reset
expect "the index digest and a platform digest inside an index both match" 0 "$two_pins"

reset
echo "$D3" >"$FAKE_DIR/acme_a_1.0"
expect "a tag that resolves to another digest is a mismatch" 1 "$two_pins"

reset
echo "ERROR: docker.io/acme/a:1.0: not found" >"$FAKE_DIR/acme_a_1.0.err"
expect "a tag the registry says is not found is a mismatch" 1 "$two_pins"

reset
echo "ERROR: GET https://registry/v2/acme/a/manifests/1.0: MANIFEST_UNKNOWN: manifest unknown" >"$FAKE_DIR/acme_a_1.0.err"
expect "a tag the registry calls an unknown manifest is a mismatch" 1 "$two_pins"

reset
echo "ERROR: dial tcp: i/o timeout" >"$FAKE_DIR/acme_a_1.0.err"
expect "a lookup that fails on both tries leaves the check incomplete" 2 "$two_pins"

reset
echo "ERROR: toomanyrequests: rate limit" >"$FAKE_DIR/acme_a_1.0.once.err"
expect "a lookup that fails once and then answers is checked" 0 "$two_pins"

reset
: >"$FAKE_DIR/acme_a_1.0"
expect "an empty answer leaves the check incomplete" 2 "$two_pins"

reset
expect "an image line with a trailing comment is reported, not skipped" 2 "$two_pins  c:
    image: acme/b:2.0@$D1  # refreshed
"

reset
expect "an image with a tag and no digest is reported, not skipped" 2 "$two_pins  c:
    image: acme/c:3.0
"

reset
echo "$D3" >"$FAKE_DIR/acme_b_2.0"
expect "a last image line with no trailing newline is still checked" 1 "${two_pins%$'\n'}"

reset
expect "a file with no image lines cannot be checked" 2 "services: {}
"

if [ "$failures" -gt 0 ]; then
	echo "$failures check-image-pins cases failed" >&2
	exit 1
fi
