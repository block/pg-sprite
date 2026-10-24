#!/usr/bin/env bash
#
# Fetch a replay project's schema-change corpus: the migration files listed
# in replay/<project>/project.conf, from the project's public repository at
# its pinned commit. Files land in replay/<project>/corpus/ (gitignored —
# the corpus belongs to the source project; re-fetch instead of vendoring).
#
#   ./fetch.sh <project>                 download the pinned corpus
#   ./fetch.sh <project> refresh [ref]   report corpus drift beyond the pin
set -euo pipefail

REPLAY_DIR="$(cd "$(dirname "$0")" && pwd)"
. "${REPLAY_DIR}/common.sh"
load_project "${1:-}"
shift

BASE_URL="https://raw.githubusercontent.com/${REPO_SLUG}/${COMMIT}/${MIGRATIONS_PATH}"

# refresh [ref] — compare the pinned corpus against the source repository's
# current schema-change history (default ref: the default branch) without
# touching corpus/ or the pin. Reports files added since the pin so a
# deliberate pin-bump + re-curation of assessment.tsv can follow; never
# mutates state itself.
if [ "${1:-}" = "refresh" ]; then
    command -v python3 >/dev/null 2>&1 || die "python3 is required to parse GitHub API responses"
    ref="${2:-HEAD}"
    api="https://api.github.com/repos/${REPO_SLUG}"
    sha="$(curl -fsSL "${api}/commits/${ref}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])')"
    echo "pinned  ${REPO_SLUG}@${COMMIT:0:12}"
    echo "current ${REPO_SLUG}@${sha:0:12} (${ref})"
    remote="$(curl -fsSL "${api}/contents/${MIGRATIONS_PATH}?ref=${sha}" \
        | python3 -c 'import json,sys; print("\n".join(sorted(e["name"] for e in json.load(sys.stdin) if e["name"].endswith(".sql"))))')"
    new=0
    while IFS= read -r f; do
        if ! printf '%s\n' "${FILES[@]}" | grep -qxF "$f"; then
            echo "new     $f"
            new=$((new + 1))
        fi
    done <<<"$remote"
    for f in "${FILES[@]}"; do
        if ! printf '%s' "$remote" | grep -qxF "$f"; then
            echo "missing $f (in pin, gone at ${ref})"
        fi
    done
    if [ "$new" -eq 0 ]; then
        echo
        echo "corpus is current: no schema-change files beyond the pin"
    else
        echo
        echo "$new new file(s): bump COMMIT to ${sha}, extend FILES, re-fetch,"
        echo "and re-curate assessment.tsv against the new pin before replaying"
    fi
    exit 0
fi

mkdir -p "${PROJECT_DIR}/corpus"

# corpus/.pin couples the corpus on disk to COMMIT: the resolved commit on
# the first line, then one "<sha256>  <file>" per corpus file. A cached file
# is trusted only when its digest matches that record, and a pin move
# discards the whole cache — so the pin identifies the corpus, not just the
# last thing typed into project.conf. replay.sh refuses a corpus whose
# record disagrees with COMMIT (verify_corpus_pin in common.sh).
PIN_FILE="${PROJECT_DIR}/corpus/.pin"
pinned=""
[ -s "$PIN_FILE" ] && pinned="$(head -n1 "$PIN_FILE")"
if [ -n "$pinned" ] && [ "$pinned" != "$COMMIT" ]; then
    echo "pin moved ${pinned:0:12} -> ${COMMIT:0:12}: discarding the cached corpus"
    for f in "${FILES[@]}"; do
        rm -f "${PROJECT_DIR}/corpus/$f"
    done
    rm -f "$PIN_FILE"
    pinned=""
fi

# Digest recorded for a file at the current pin; fails when there is none.
recorded_digest() {
    [ -n "$pinned" ] || return 1
    awk -v f="$1" '$2 == f {print $1; found=1} END {exit !found}' "$PIN_FILE"
}

for f in "${FILES[@]}"; do
    if want="$(recorded_digest "$f")" \
        && [ "$(file_sha256 "${PROJECT_DIR}/corpus/$f")" = "$want" ]; then
        echo "have  $f"
        continue
    fi
    echo "fetch $f"
    curl -fsSL --retry 3 -o "${PROJECT_DIR}/corpus/$f.tmp" "${BASE_URL}/${f}"
    mv "${PROJECT_DIR}/corpus/$f.tmp" "${PROJECT_DIR}/corpus/$f"
done

{
    echo "$COMMIT"
    for f in "${FILES[@]}"; do
        echo "$(file_sha256 "${PROJECT_DIR}/corpus/$f")  $f"
    done
} >"$PIN_FILE"

echo
echo "corpus complete: ${#FILES[@]} files at ${REPO_SLUG}@${COMMIT:0:12}"
