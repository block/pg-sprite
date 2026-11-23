#!/bin/bash
#
# Fail when a compose image pin's tag no longer names its digest.
#
# Each pin has the form `image: repo:tag@sha256:<digest>`. Docker pulls by the
# digest and ignores the tag, so a refresh that pairs the wrong tag with a valid
# digest still starts, still passes CI, and leaves the tested versions in
# docs/supabase.md naming an image that never ran. This asks the registry what
# the tag resolves to now and compares that with the pinned digest.
#
# A pin may name either the multi-arch index or one platform's manifest inside
# it, so a digest matches when it is the tag's top-level digest or one of the
# manifests that digest lists. A tag that names a single manifest has only the
# top-level digest.
#
# Every `image:` line must be a pin in that form, on its own: a line the check
# cannot read (a trailing comment, a variable, a tag with no digest) is
# reported as unchecked rather than skipped, so "all pins resolve" covers every
# image in the file.
#
# A tag can be moved on purpose by its publisher, so a failure here does not
# always mean the pin was written wrong; it means the pinned version and the
# tag disagree and one of them has to be corrected before the docs can be
# trusted. The remedy is a pin change, reviewed like any other image update.
#
# Exit 1 means at least one pin's tag does not resolve to its digest (or the
# tag no longer exists). Exit 2 means every pin the check could ask about
# matched but at least one could not be checked: its registry lookup failed
# (unreachable, rate-limited, or not authorized) or its line is not a pin the
# check can read. It is also the exit when the check cannot run at all (no
# buildx, no pins). Each lookup is tried twice, with a time limit when
# `timeout` is installed. Public Docker Hub, ghcr.io, and public.ecr.aws images
# need no login; a private image needs `docker login` to its registry first.
#
# Usage: scripts/check-image-pins.sh [compose-file]   (default compose/supabase.yml)

set -euo pipefail

compose_file="${1:-$(git rev-parse --show-toplevel)/compose/supabase.yml}"
if [ ! -f "$compose_file" ]; then
	echo "compose file not found: $compose_file" >&2
	exit 2
fi
if ! docker buildx version >/dev/null 2>&1; then
	echo "docker buildx is required to ask registries for a tag's digest" >&2
	exit 2
fi

image_pattern='^[[:space:]]*image:'
pin_pattern='^[[:space:]]*image:[[:space:]]*["'\'']?([^"'\''@[:space:]]+)@(sha256:[0-9a-f]{64})["'\'']?[[:space:]]*$'

# The top-level digest first, then, for an index, the digest of every manifest
# it lists. A single manifest has no list to range over.
inspect_format='{{.Manifest.Digest}}{{if or (eq .Manifest.MediaType "application/vnd.oci.image.index.v1+json") (eq .Manifest.MediaType "application/vnd.docker.distribution.manifest.list.v2+json")}}{{range .Manifest.Manifests}} {{.Digest}}{{end}}{{end}}'
lookup_seconds=60

errors_file=$(mktemp)
trap 'rm -f "$errors_file"' EXIT

# lookup prints the digests the tag resolves to on stdout and leaves the
# registry's error text in $errors_file when the lookup fails.
lookup() {
	local cmd=(docker buildx imagetools inspect "$1" --format "$inspect_format")
	if command -v timeout >/dev/null 2>&1; then
		cmd=(timeout "$lookup_seconds" "${cmd[@]}")
	fi
	"${cmd[@]}" 2>"$errors_file" </dev/null
}

# tag_missing reports whether a failed lookup's error says the tag is gone,
# as opposed to the registry not answering or refusing to.
tag_missing() {
	grep -q -e ': not found' -e 'manifest unknown' "$errors_file"
}

pins=0
line_number=0
mismatched=()
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
	pinned="${BASH_REMATCH[2]}"
	pins=$((pins + 1))

	# A second try absorbs a transient registry error; a missing tag is final.
	resolved=""
	for attempt in 1 2; do
		if resolved=$(lookup "$ref") && [ -n "$resolved" ]; then
			break
		fi
		resolved=""
		if tag_missing; then
			break
		fi
	done
	if [ -z "$resolved" ]; then
		if tag_missing; then
			mismatched+=("$ref@$pinned: the tag does not exist in the registry")
		else
			unchecked+=("$ref: the registry lookup failed after $attempt tries: $(tr '\n' ' ' <"$errors_file")")
		fi
		continue
	fi

	read -r -a digests <<<"$resolved"
	matched=false
	for digest in "${digests[@]}"; do
		if [ "$digest" = "$pinned" ]; then
			matched=true
			break
		fi
	done
	if [ "$matched" = true ]; then
		echo "ok      $ref -> $pinned"
	else
		mismatched+=("$ref@$pinned: the tag resolves to ${digests[0]}")
	fi
done <"$compose_file"

if [ "$pins" -eq 0 ] && [ "${#unchecked[@]}" -eq 0 ]; then
	echo "no image: repo:tag@sha256:... pins found in $compose_file" >&2
	exit 2
fi

for entry in "${unchecked[@]+"${unchecked[@]}"}"; do
	echo "UNCHECKED    $entry" >&2
done
for entry in "${mismatched[@]+"${mismatched[@]}"}"; do
	echo "MISMATCH     $entry" >&2
done

if [ "${#mismatched[@]}" -gt 0 ]; then
	echo "${#mismatched[@]} pins in $compose_file do not match their tag; correct the tag or the digest so both name the image that was tested" >&2
	exit 1
fi
if [ "${#unchecked[@]}" -gt 0 ]; then
	echo "${#unchecked[@]} images in $compose_file could not be checked; the check is incomplete for them" >&2
	exit 2
fi
echo "all $pins pins in $compose_file resolve their tag to the pinned digest"
