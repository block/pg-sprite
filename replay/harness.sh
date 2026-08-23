#!/usr/bin/env bash
#
# Throwaway PostgreSQL harness for a corpus replay: starts the project's
# pinned postgres image, applies the project's baseline file via psql as the
# starting state, and resets to exactly that state on demand. All state
# lives inside the container — `reset` and `down` are always safe. This
# never points at any real environment.
#
#   ./harness.sh <project> up        start the container, apply the baseline
#   ./harness.sh <project> reset     drop and recreate the DB, re-apply baseline
#   ./harness.sh <project> psql ...  run psql inside the container
#   ./harness.sh <project> dsn       print the DSN for pg-sprite / host clients
#   ./harness.sh <project> down      remove the container and all its state
set -euo pipefail

REPLAY_DIR="$(cd "$(dirname "$0")" && pwd)"
. "${REPLAY_DIR}/common.sh"
load_project "${1:-}"
shift

require_baseline() {
    [ -z "$BASELINE" ] && return 0
    [ -s "${PROJECT_DIR}/corpus/${BASELINE}" ] \
        || die "missing corpus/${BASELINE} — run ./fetch.sh ${PROJECT} first"
}

wait_ready() {
    for _ in $(seq 1 60); do
        if docker exec "$CONTAINER" pg_isready -U "$DB_NAME" -d postgres >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    die "database in container $CONTAINER not ready after 60s"
}

apply_baseline() {
    if [ -z "$BASELINE" ]; then
        echo "no baseline configured: starting from an empty database"
        return 0
    fi
    echo "applying baseline corpus/${BASELINE}"
    docker exec -i "$CONTAINER" psql -q -U "$DB_NAME" -d "$DB_NAME" \
        -v ON_ERROR_STOP=1 <"${PROJECT_DIR}/corpus/${BASELINE}"
    echo "baseline applied"
}

recreate_database() {
    docker exec "$CONTAINER" psql -q -U "$DB_NAME" -d postgres -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" \
        -c "CREATE DATABASE ${DB_NAME} OWNER ${DB_NAME}"
}

cmd_up() {
    require_baseline
    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        die "container $CONTAINER already exists — use reset, or down first"
    fi
    # Test-only speed settings, mirroring compose/compose.yml; never carry
    # these to a real deployment.
    docker run -d --name "$CONTAINER" \
        -e POSTGRES_USER="$DB_NAME" \
        -e POSTGRES_PASSWORD="$DB_NAME" \
        -e POSTGRES_DB="$DB_NAME" \
        -p "${PORT}:5432" \
        "$PG_IMAGE" postgres -c fsync=off -c full_page_writes=off >/dev/null
    wait_ready
    apply_baseline
    echo
    cmd_dsn
}

cmd_reset() {
    require_baseline
    docker inspect "$CONTAINER" >/dev/null 2>&1 || die "container $CONTAINER not running — use up"
    wait_ready
    recreate_database
    apply_baseline
}

cmd_psql() {
    docker exec -i "$CONTAINER" psql -U "$DB_NAME" -d "$DB_NAME" "$@"
}

cmd_dsn() {
    echo "postgres://${DB_NAME}:${DB_NAME}@localhost:${PORT}/${DB_NAME}?sslmode=disable"
}

cmd_down() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    echo "removed $CONTAINER"
}

case "${1:-}" in
    up)    cmd_up ;;
    reset) cmd_reset ;;
    psql)  shift; cmd_psql "$@" ;;
    dsn)   cmd_dsn ;;
    down)  cmd_down ;;
    *)     die "usage: $0 <project> {up|reset|psql|dsn|down}" ;;
esac
