#!/bin/bash
#
# Drive scripts/check-guide-versions.sh through each outcome. Each case writes
# a compose file and a guide, then checks the script's exit code and, for a
# missing version, that the failure names the pin.
#
# Usage: scripts/check-guide-versions_test.sh

set -euo pipefail

scripts="$(cd "$(dirname "$0")" && pwd)"
script="$scripts/check-guide-versions.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

D1=$(printf 'sha256:%064d' 1)

failures=0
# expect NAME WANT_EXIT WANT_OUTPUT COMPOSE_CONTENT GUIDE_CONTENT: run the check
# on a fresh compose file and guide. WANT_OUTPUT, when not empty, is a line
# the output must contain.
expect() {
	local name="$1" want="$2" want_output="$3"
	printf '%s' "$4" >"$work/compose.yml"
	printf '%s' "$5" >"$work/guide.md"
	local got=0
	"$script" "$work/compose.yml" "$work/guide.md" >"$work/out" 2>&1 || got=$?
	if [ "$got" -ne "$want" ]; then
		echo "FAIL $name: exit $got, want $want" >&2
		sed 's/^/    /' "$work/out" >&2
		failures=$((failures + 1))
	elif [ -n "$want_output" ] && ! grep -qxF -- "$want_output" "$work/out"; then
		echo "FAIL $name: output lacks: $want_output" >&2
		sed 's/^/    /' "$work/out" >&2
		failures=$((failures + 1))
	else
		echo "ok   $name"
	fi
}
compose="services:
  db:
    image: acme/postgres:17.6.1.136@$D1
  auth:
    image: \"acme/gotrue:v2.197.0@$D1\"
"
guide_with() {
	printf '# Guide\n\n## What works today\n\nA local `acme/postgres:17.6.1.136` database\nwas exercised. Auth %s was tested.\n\n## Later\n\nAuth 2.198.0 is planned.\n' "$1"
}

expect "a guide naming every pinned version passes, with the tag's v dropped" 0 "" \
	"$compose" "$(guide_with 2.197.0)"

expect "a version that ends a sentence still counts" 0 "" \
	"$compose" "$(guide_with "2.197.0.")"

# The guide names 2.198.0 only in a later section, which is not a tested version.
expect "a bumped tag the tested versions do not name fails, naming the pin" 1 \
	"MISSING      acme/gotrue:v2.198.0: the guide's tested versions do not name 2.198.0" \
	"${compose/v2.197.0/v2.198.0}" "$(guide_with 2.197.0)"

expect "a bumped tag passes once the tested versions name it" 0 "" \
	"${compose/v2.197.0/v2.198.0}" "$(guide_with 2.198.0)"

expect "a longer version does not stand in for the pinned one" 1 \
	"MISSING      acme/gotrue:v2.197.0: the guide's tested versions do not name 2.197.0" \
	"$compose" "$(guide_with 2.197.0.1)"

expect "a version inside another version does not stand in for it" 1 \
	"MISSING      acme/gotrue:v2.197.0: the guide's tested versions do not name 2.197.0" \
	"$compose" "$(guide_with 12.197.0)"

expect "a guide without the tested-versions heading cannot be checked" 2 "" \
	"$compose" "# Guide

Auth 2.197.0.
"

expect "an image line the check cannot read is reported, not skipped" 2 "" \
	"$compose  c:
    image: acme/c:3.0
" "$(guide_with 2.197.0)"

expect "a file with no image lines cannot be checked" 2 "" \
	"services: {}
" "$(guide_with 2.197.0)"

if [ "$failures" -gt 0 ]; then
	echo "$failures check-guide-versions cases failed" >&2
	exit 1
fi
