#!/usr/bin/env bash
#
# Throwaway PostgreSQL harness for the buzz corpus replay: starts a
# postgres:17-alpine container (the major buzz pins in its own compose
# setup), applies corpus file 0001 via psql as the baseline starting state,
# and resets to exactly that state on demand. All state lives inside the
# container — `reset` and `down` are always safe. This never points at any
# real environment.
#
#   ./harness.sh up        start the container and apply the baseline
#   ./harness.sh reset     drop and recreate the database, re-apply baseline
#   ./harness.sh psql ...  run psql inside the container (interactive or -c)
#   ./harness.sh dsn       print the DSN for pg-sprite / clients on the host
#   ./harness.sh down      remove the container and all its state
set -euo pipefail

cd "$(dirname "$0")"

CONTAINER="${CONTAINER:-pgsprite-buzz-replay}"
IMAGE="postgres:17-alpine"
PORT="${PORT:-5439}"
DB_USER="buzz"
DB_PASSWORD="buzz"
DB_NAME="buzz"

baseline_file="corpus/0001_initial_schema.sql"

die() { echo "$*" >&2; exit 1; }

require_corpus() {
    [ -s "$baseline_file" ] || die "missing $baseline_file — run ./fetch.sh first"
}

wait_ready() {
    for _ in $(seq 1 60); do
        if docker exec "$CONTAINER" pg_isready -U "$DB_USER" -d postgres >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    die "database in container $CONTAINER not ready after 60s"
}

apply_baseline() {
    echo "applying baseline $baseline_file"
    docker exec -i "$CONTAINER" psql -q -U "$DB_USER" -d "$DB_NAME" \
        -v ON_ERROR_STOP=1 <"$baseline_file"
    echo "baseline applied"
}

recreate_database() {
    docker exec "$CONTAINER" psql -q -U "$DB_USER" -d postgres -v ON_ERROR_STOP=1 \
        -c "DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)" \
        -c "CREATE DATABASE ${DB_NAME} OWNER ${DB_USER}"
}

cmd_up() {
    require_corpus
    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
        die "container $CONTAINER already exists — use reset, or down first"
    fi
    # Test-only speed settings, mirroring compose/compose.yml; never carry
    # these to a real deployment.
    docker run -d --name "$CONTAINER" \
        -e POSTGRES_USER="$DB_USER" \
        -e POSTGRES_PASSWORD="$DB_PASSWORD" \
        -e POSTGRES_DB="$DB_NAME" \
        -p "${PORT}:5432" \
        "$IMAGE" postgres -c fsync=off -c full_page_writes=off >/dev/null
    wait_ready
    apply_baseline
    echo
    cmd_dsn
}

cmd_reset() {
    require_corpus
    docker inspect "$CONTAINER" >/dev/null 2>&1 || die "container $CONTAINER not running — use up"
    wait_ready
    recreate_database
    apply_baseline
}

cmd_psql() {
    docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" "$@"
}

cmd_dsn() {
    echo "postgres://${DB_USER}:${DB_PASSWORD}@localhost:${PORT}/${DB_NAME}?sslmode=disable"
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
    *)     die "usage: $0 {up|reset|psql|dsn|down}" ;;
esac
