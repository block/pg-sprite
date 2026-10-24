# Shared plumbing for the corpus replay scripts: project loading and
# validation. Sourced by fetch.sh / harness.sh / replay.sh, never executed.
# Callers set REPLAY_DIR to this directory before sourcing.

die() { echo "$*" >&2; exit 1; }

# sha256 of a file, portable across sha256sum (Linux) and shasum (macOS).
# Fails (non-zero, no output) when the file is missing.
file_sha256() {
    [ -f "$1" ] || return 1
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# verify_corpus_pin: refuse to proceed unless corpus/.pin (written by
# fetch.sh) records exactly the commit project.conf pins, with every corpus
# file matching its fetched digest. This is the interlock that makes the pin
# a guarantee rather than a label: an assessment must never apply to a
# corpus it was not written against, and a cached file from an older pin —
# or a locally edited one — must fail loudly, not replay under today's label.
verify_corpus_pin() {
    local pin="${PROJECT_DIR}/corpus/.pin"
    [ -s "$pin" ] || die "corpus has no pin record — run ./fetch.sh ${PROJECT} first"
    local fetched
    fetched="$(head -n1 "$pin")"
    [ "$fetched" = "$COMMIT" ] \
        || die "corpus was fetched at ${fetched:0:12} but project.conf pins ${COMMIT:0:12} — run ./fetch.sh ${PROJECT}"
    local f want got
    for f in "${FILES[@]}"; do
        want="$(awk -v f="$f" '$2 == f {print $1}' "$pin")"
        [ -n "$want" ] || die "corpus/.pin records no digest for $f — run ./fetch.sh ${PROJECT}"
        got="$(file_sha256 "${PROJECT_DIR}/corpus/$f")" \
            || die "missing corpus/$f — run ./fetch.sh ${PROJECT}"
        [ "$got" = "$want" ] \
            || die "corpus/$f does not match what fetch.sh fetched (edited locally?) — run ./fetch.sh ${PROJECT}"
    done
}

# The project name doubles as the directory, container suffix, and the
# database/user/password inside the throwaway container, so it must be usable
# in all of those. Identifier quoting keeps hyphens safe in SQL; this bounds
# the rest (URLs, docker names, filenames) to one predictable charset.
validate_project_name() {
    case "$1" in
        ''|*[!a-z0-9_-]*|[-_]*) die "project name must be lowercase letters, digits, hyphen, or underscore, starting with a letter or digit: $1" ;;
    esac
}

# load_project <name>: source replay/<name>/project.conf and validate it.
# Sets PROJECT and PROJECT_DIR, the conf variables (REPO, COMMIT,
# MIGRATIONS_PATH, PG_IMAGE, PORT, BASELINE, FILES), and the derived
# REPO_SLUG (owner/name), CONTAINER, and DB_NAME.
load_project() {
    local name="${1:-}"
    [ -n "$name" ] || die "usage: $(basename "$0") <project> ... — see replay/README.md"
    PROJECT="$(basename "$name")"
    validate_project_name "$PROJECT"
    PROJECT_DIR="${REPLAY_DIR}/${PROJECT}"
    local conf="${PROJECT_DIR}/project.conf"
    [ -s "$conf" ] || die "no replay project at ${PROJECT_DIR} (missing project.conf) — see replay/README.md"

    REPO="" COMMIT="" MIGRATIONS_PATH="" PG_IMAGE="" PORT="" BASELINE=""
    FILES=()
    # shellcheck source=/dev/null
    . "$conf"

    local v
    for v in REPO COMMIT MIGRATIONS_PATH PG_IMAGE PORT; do
        [ -n "${!v}" ] || die "${conf}: ${v} is required"
    done
    [ "${#FILES[@]}" -gt 0 ] || die "${conf}: FILES must list the corpus files"

    # REPO accepts owner/name or a github.com URL; normalize to owner/name.
    REPO_SLUG="${REPO#https://github.com/}"
    REPO_SLUG="${REPO_SLUG#http://github.com/}"
    REPO_SLUG="${REPO_SLUG%.git}"
    REPO_SLUG="${REPO_SLUG%/}"
    case "$REPO_SLUG" in
        */*) ;;
        *) die "${conf}: REPO must be owner/name or a github.com URL, got: ${REPO}" ;;
    esac

    # Throwaway container-local identity: the project name doubles as the
    # database, user, and password. Never carries to a real environment.
    CONTAINER="${CONTAINER:-pgsprite-${PROJECT}-replay}"
    DB_NAME="$PROJECT"
}
