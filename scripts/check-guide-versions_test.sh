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
	# The guide is written with the newline $(guide_with ...) strips, so its last
	# line is read like any other.
	printf '%s\n' "$5" >"$work/guide.md"
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
compose_bumped="${compose/v2.197.0/v2.198.0}"
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

expect "a pre-release of the pinned version does not stand in for it" 1 \
	"MISSING      acme/gotrue:v2.197.0: the guide's tested versions do not name 2.197.0" \
	"$compose" "$(guide_with 2.197.0-rc1)"

expect "a build of the pinned version does not stand in for it" 1 \
	"MISSING      acme/gotrue:v2.197.0: the guide's tested versions do not name 2.197.0" \
	"$compose" "$(guide_with 2.197.0+build5)"

expect "a last image line with no trailing newline is still checked" 1 \
	"MISSING      acme/gotrue:v2.198.0: the guide's tested versions do not name 2.198.0" \
	"${compose_bumped%$'\n'}" "$(guide_with 2.197.0)"

# A line of only spaces is a blank line in Markdown, so the version after it
# is in a second paragraph.
expect "a line of spaces ends the tested-versions paragraph" 1 \
	"MISSING      acme/gotrue:v2.197.0: the guide's tested versions do not name 2.197.0" \
	"$compose" "# Guide

## What works today

A local \`acme/postgres:17.6.1.136\` database was exercised.
   
Auth 2.197.0 was tested."

expect "a heading with trailing spaces and a CRLF ending is still the heading" 0 "" \
	"$compose" "# Guide

## What works today  "$'\r'"

A local \`acme/postgres:17.6.1.136\` database. Auth 2.197.0 was tested."

expect "a guide without the tested-versions heading says the heading is missing" 2 \
	"heading \"## What works today\" not found in $work/guide.md; the tested versions go in the first paragraph under it" \
	"$compose" "# Guide

Auth 2.197.0.
"

expect "a heading with nothing under it says the paragraph is missing" 2 \
	"no paragraph under \"## What works today\" in $work/guide.md to hold the tested versions" \
	"$compose" "# Guide

## What works today
"

# Both paths are given, so the check needs no git checkout to find them.
printf '%s' "$compose" >"$work/compose.yml"
guide_with 2.197.0 >"$work/guide.md"
got=0
(cd "$work" && GIT_CEILING_DIRECTORIES="$work" "$script" "$work/compose.yml" "$work/guide.md") >"$work/out" 2>&1 || got=$?
if [ "$got" -ne 0 ]; then
	echo "FAIL the check runs outside a git checkout when both paths are given: exit $got, want 0" >&2
	sed 's/^/    /' "$work/out" >&2
	failures=$((failures + 1))
else
	echo "ok   the check runs outside a git checkout when both paths are given"
fi

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
