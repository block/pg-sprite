# Shared plumbing for the corpus replay scripts: project loading and
# validation. Sourced by fetch.sh / harness.sh / replay.sh, never executed.
# Callers set REPLAY_DIR to this directory before sourcing.

die() { echo "$*" >&2; exit 1; }

# load_project <name>: source replay/<name>/project.conf and validate it.
# Sets PROJECT and PROJECT_DIR, the conf variables (REPO, COMMIT,
# MIGRATIONS_PATH, PG_IMAGE, PORT, BASELINE, FILES), and the derived
# REPO_SLUG (owner/name), CONTAINER, and DB_NAME.
load_project() {
    local name="${1:-}"
    [ -n "$name" ] || die "usage: $(basename "$0") <project> ... — see replay/README.md"
    PROJECT="$(basename "$name")"
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
