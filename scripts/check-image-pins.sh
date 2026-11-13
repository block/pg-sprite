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
# manifests that digest lists.
#
# A tag can be moved on purpose by its publisher, so a failure here does not
# always mean the pin was written wrong; it means the pinned version and the
# tag disagree and one of them has to be corrected before the docs can be
# trusted. The remedy is a pin change, reviewed like any other image update.
#
# Exit 1 means at least one pin's tag does not resolve to its digest (or the
# tag no longer exists). Exit 2 means every reachable pin matched but at least
# one registry could not be asked, so the check is incomplete for those pins;
# it is also the exit when the check cannot run at all (no buildx, no pins).
# Public Docker Hub, ghcr.io, and public.ecr.aws images need no login; the
# lookup uses any credentials docker already has and works without them.
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

pin_pattern='^[[:space:]]*image:[[:space:]]*["'\'']?([^"'\''@[:space:]]+)@(sha256:[0-9a-f]{64})["'\'']?[[:space:]]*$'

pins=0
mismatched=()
unreachable=()
while IFS= read -r line; do
	if [[ ! "$line" =~ $pin_pattern ]]; then
		continue
	fi
	ref="${BASH_REMATCH[1]}"
	pinned="${BASH_REMATCH[2]}"
	pins=$((pins + 1))

	# The top-level digest first, then the digest of every manifest an index lists.
	if ! resolved=$(docker buildx imagetools inspect "$ref" \
		--format '{{.Manifest.Digest}}{{range .Manifest.Manifests}} {{.Digest}}{{end}}' 2>&1); then
		if [[ "$resolved" == *": not found"* ]]; then
			mismatched+=("$ref@$pinned: the tag does not exist in the registry")
		else
			unreachable+=("$ref: ${resolved//$'\n'/ }")
		fi
		continue
	fi

	read -r -a digests < <(printf '%s\n' "$resolved")
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

if [ "$pins" -eq 0 ]; then
	echo "no image: repo:tag@sha256:... pins found in $compose_file" >&2
	exit 2
fi

for entry in "${unreachable[@]+"${unreachable[@]}"}"; do
	echo "unreachable  $entry" >&2
done
for entry in "${mismatched[@]+"${mismatched[@]}"}"; do
	echo "MISMATCH     $entry" >&2
done

if [ "${#mismatched[@]}" -gt 0 ]; then
	echo "${#mismatched[@]} of $pins pins in $compose_file do not match their tag; correct the tag or the digest so both name the image that was tested" >&2
	exit 1
fi
if [ "${#unreachable[@]}" -gt 0 ]; then
	echo "could not reach the registry for ${#unreachable[@]} of $pins pins in $compose_file; the check is incomplete for them" >&2
	exit 2
fi
echo "all $pins pins in $compose_file resolve their tag to the pinned digest"
