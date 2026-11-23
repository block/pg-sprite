#!/bin/bash
#
# Fail when the Supabase guide's tested versions do not name a compose image
# pin's version.
#
# Each pin has the form `image: repo:tag@sha256:<digest>`, and the paragraph
# under the guide's "What works today" heading states the versions that were
# tested. check-image-pins.sh proves a tag names its digest; this proves the
# guide names the tag, so a refresh that moves a tag and its digest together
# cannot leave the guide naming a version that never ran. A tag's leading `v`
# is dropped (`v2.197.0` is written `2.197.0`), and the version must stand as
# its own word: `2.10` does not match `2.10.1` or `2.10-rc1`, but may end a
# sentence. Only the first paragraph under the heading is read.
#
# Reads only the two files: no registry, no Docker. Every `image:` line must
# be a pin in that form, on its own, as check-image-pins.sh also requires.
#
# Exit 1 means at least one pin's version is missing from the paragraph. Exit 2
# means the check could not run or is incomplete: a file is missing, the
# paragraph is not found, there are no pins, or an `image:` line is not a pin
# the check can read.
#
# Usage: scripts/check-guide-versions.sh [compose-file] [guide]
#        (defaults compose/supabase.yml and docs/supabase.md)

set -euo pipefail

# The defaults ask git for the checkout only when a path is not given, so the
# check runs outside a checkout when both are.
compose_file="${1:-$(git rev-parse --show-toplevel)/compose/supabase.yml}"
guide="${2:-$(git rev-parse --show-toplevel)/docs/supabase.md}"
for file in "$compose_file" "$guide"; do
	if [ ! -f "$file" ]; then
		echo "file not found: $file" >&2
		exit 2
	fi
done

heading='## What works today'
image_pattern='^[[:space:]]*image:'
pin_pattern='^[[:space:]]*image:[[:space:]]*["'\'']?([^"'\''@[:space:]]+)@(sha256:[0-9a-f]{64})["'\'']?[[:space:]]*$'

# is_heading reports whether a guide line is the heading, ignoring trailing
# spaces and a CRLF line ending.
is_heading() {
	local line="$1"
	[ "${line%"${line##*[![:space:]]}"}" = "$heading" ]
}

# has_heading reports whether the guide has the heading at all.
has_heading() {
	local line
	while IFS= read -r line || [ -n "$line" ]; do
		if is_heading "$line"; then
			return 0
		fi
	done <"$guide"
	return 1
}

# tested_paragraph prints the first paragraph after the heading, on one line,
# so a version is found wherever the prose wraps. A line of only spaces ends
# the paragraph, as it does in Markdown.
tested_paragraph() {
	local in_section=false started=false paragraph=""
	while IFS= read -r line || [ -n "$line" ]; do
		if [ "$in_section" = false ]; then
			if is_heading "$line"; then
				in_section=true
			fi
			continue
		fi
		if [ -z "${line//[[:space:]]/}" ]; then
			if [ "$started" = true ]; then
				break
			fi
			continue
		fi
		started=true
		paragraph="$paragraph $line"
	done <"$guide"
	printf '%s\n' "$paragraph"
}

# names_version reports whether the paragraph holds the version as a word of
# its own: not preceded by a digit or dot, and not followed by a digit, a
# dot and a digit, or a pre-release or build suffix (`2.197.0-rc1` and
# `2.197.0+build5` are other releases).
names_version() {
	local escaped="${1//./[.]}"
	local word="(^|[^0-9.])${escaped}([^0-9.+-]|[.]([^0-9]|$)|$)"
	[[ "$2" =~ $word ]]
}

if ! has_heading; then
	echo "heading \"$heading\" not found in $guide; the tested versions go in the first paragraph under it" >&2
	exit 2
fi
paragraph=$(tested_paragraph)
if [ -z "${paragraph//[[:space:]]/}" ]; then
	echo "no paragraph under \"$heading\" in $guide to hold the tested versions" >&2
	exit 2
fi

pins=0
line_number=0
missing=()
unchecked=()
while IFS= read -r line || [ -n "$line" ]; do
	line_number=$((line_number + 1))
	if [[ ! "$line" =~ $image_pattern ]]; then
		continue
	fi
	if [[ ! "$line" =~ $pin_pattern ]]; then
		unchecked+=("line $line_number is not an image: repo:tag@sha256:... pin the check can read: ${line#"${line%%[![:space:]]*}"}")
		continue
	fi
	ref="${BASH_REMATCH[1]}"
	pins=$((pins + 1))
	name="${ref##*/}"
	if [[ "$name" != *:* ]]; then
		unchecked+=("$ref has no tag to name a version")
		continue
	fi
	tag="${ref##*:}"
	version="${tag#v}"
	if names_version "$version" "$paragraph"; then
		echo "ok      $ref -> $version"
	else
		missing+=("$ref: the guide's tested versions do not name $version")
	fi
done <"$compose_file"

if [ "$pins" -eq 0 ] && [ "${#unchecked[@]}" -eq 0 ]; then
	echo "no image: repo:tag@sha256:... pins found in $compose_file" >&2
	exit 2
fi

for entry in "${unchecked[@]+"${unchecked[@]}"}"; do
	echo "UNCHECKED    $entry" >&2
done
for entry in "${missing[@]+"${missing[@]}"}"; do
	echo "MISSING      $entry" >&2
done

if [ "${#missing[@]}" -gt 0 ]; then
	echo "${#missing[@]} pins in $compose_file are not named under \"$heading\" in $guide; update the tested versions to the pinned ones; they are read from the first paragraph under the heading only" >&2
	exit 1
fi
if [ "${#unchecked[@]}" -gt 0 ]; then
	echo "${#unchecked[@]} images in $compose_file could not be checked against $guide" >&2
	exit 2
fi
echo "all $pins pinned versions in $compose_file are named in $guide"
