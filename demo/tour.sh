#!/usr/bin/env bash
#
# A runnable tour of the pg-sprite CLI: walk one statement through every
# planner route and reason, print the declarative diff plans, run the
# offline commands, and finish by executing real schema changes — including
# the safer-sequence substitutions — against the seeded demo tables. Run it
# via `make demo`, which builds the binary, starts the compose database,
# and reseeds demo/seed.sql first.
#
# CHECK=1 turns the tour into the packaged-binary smoke test CI runs: the
# same commands, asserting only on --json fields and exit codes — never on
# human-facing output, which is free to change. Check mode needs jq.
#
# The expectation rows below are a contract with the planner: when a change
# adds, renames, or removes a route, reason, verdict outcome, or safer
# idiom, extend the rows in the same PR (see AGENTS.md).
set -euo pipefail

PGS="${PGS:?set PGS to the pg-sprite binary path}"
PG_DSN="${PG_DSN:?set PG_DSN to the demo database URL}"
CHECK="${CHECK:-0}"
section="${1:-all}"

cd "$(dirname "$0")"

if [ "$CHECK" = 1 ] && ! command -v jq >/dev/null 2>&1; then
    echo "CHECK=1 asserts on --json output and needs jq" >&2
    exit 1
fi

failures=0

heading() { printf '\n══ %s ══\n' "$*"; }
step()    { printf '\n── %s\n' "$*"; }

fail() {
    printf 'CHECK FAIL: %s\n' "$*" >&2
    failures=$((failures + 1))
}

# assert_eq label expected actual
assert_eq() {
    if [ "$2" != "$3" ]; then
        fail "$1: expected '$2', got '$3'"
    fi
}

# dry_run route reason destructive sql
#
# Demo mode prints the routed plan. Check mode asserts the statement's
# route, first decision reason, and destructive flag from the JSON plan
# report. Dry-run never writes, so these rows can rerun in any order.
dry_run() {
    local route="$1" reason="$2" destructive="$3" sql="$4" out status=0
    step "$sql"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" migrate --url "$PG_DSN" --dry-run --json --alter "$sql") || status=$?
        assert_eq "dry-run exit of [$sql]" 0 "$status"
        assert_eq "route of [$sql]" "$route" "$(jq -r '.statements[0].route' <<<"$out")"
        assert_eq "reason of [$sql]" "$reason" "$(jq -r '.statements[0].decisions[0].reason' <<<"$out")"
        assert_eq "destructive of [$sql]" "$destructive" "$(jq -r '.statements[0].destructive' <<<"$out")"
    else
        "$PGS" migrate --url "$PG_DSN" --dry-run --alter "$sql" || echo "(exit $?)"
    fi
}

# execute_native sql
#
# Runs the schema change for real and requires the executed-natively
# verdict — this exercises the safer-sequence runner (the concurrent index
# build, the four-step SET NOT NULL) in the packaged binary.
execute_native() {
    local sql="$1" out status=0
    step "execute: $sql"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" migrate --url "$PG_DSN" --json --alter "$sql") || status=$?
        assert_eq "exit of [$sql]" 0 "$status"
        assert_eq "outcome of [$sql]" "executed-natively" "$(jq -r '.outcome' <<<"$out")"
    else
        "$PGS" migrate --url "$PG_DSN" --alter "$sql"
    fi
}

# execute_refused reason sql
#
# Runs a schema change the engine must refuse and requires the refusal
# verdict, its reason, and the distinct refusal exit code.
execute_refused() {
    local reason="$1" sql="$2" out status=0
    step "execute (expect refusal): $sql"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" migrate --url "$PG_DSN" --json --alter "$sql") || status=$?
        assert_eq "refusal exit of [$sql]" 2 "$status"
        assert_eq "outcome of [$sql]" "refused" "$(jq -r '.outcome' <<<"$out")"
        assert_eq "refusal reason of [$sql]" "$reason" "$(jq -r '.reason' <<<"$out")"
    else
        "$PGS" migrate --url "$PG_DSN" --alter "$sql" \
            || echo "(refused, exit $? — expected: this route's backend is not available yet)"
    fi
}

# diff_plan desired_file statement_count first_sql_fragment
diff_plan() {
    local desired="$1" count="$2" fragment="$3" out status=0
    step "diff --desired $desired"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" diff --url "$PG_DSN" --desired "$desired" --schema public --json) || status=$?
        assert_eq "diff exit of [$desired]" 0 "$status"
        assert_eq "statement count of [$desired]" "$count" "$(jq -r '.statements | length' <<<"$out")"
        case "$(jq -r '.statements[0].sql' <<<"$out")" in
        *"$fragment"*) ;;
        *) fail "first statement of [$desired]: expected it to contain '$fragment'" ;;
        esac
    else
        "$PGS" diff --url "$PG_DSN" --desired "$desired" --schema public || echo "(exit $?)"
    fi
}

run_dryrun() {
    heading "Classification: one statement per route and reason (dry-run, no writes)"
    #       route          reason                 destructive
    dry_run native         metadata-only          false "ALTER TABLE users ADD COLUMN bio text"
    dry_run native         fast-default           false "ALTER TABLE users ADD COLUMN tier text DEFAULT 'free'"
    dry_run native         binary-coercible       false "ALTER TABLE users ALTER COLUMN name TYPE varchar(100)"
    dry_run native         safer-idiom            false "ALTER TABLE users ALTER COLUMN email SET NOT NULL"
    dry_run native         safer-idiom            false "CREATE INDEX idx_users_email ON users (email)"
    dry_run native         online-idiom           false "CREATE INDEX CONCURRENTLY idx_users_email ON users (email)"
    dry_run native         safer-idiom            false "ALTER TABLE users ADD CONSTRAINT users_name_nonempty CHECK (char_length(name) > 0)"
    dry_run native         safer-idiom            false "ALTER TABLE users ADD CONSTRAINT users_email_uniq UNIQUE (email)"
    dry_run copy-and-swap  type-rewrite           false "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
    dry_run copy-and-swap  volatile-default       false "ALTER TABLE users ADD COLUMN joined timestamptz DEFAULT now()"
    dry_run native         app-breaking-rename    false "ALTER TABLE users RENAME COLUMN name TO full_name"
    dry_run native         metadata-only          true  "ALTER TABLE users DROP COLUMN status"
    dry_run copy-and-swap  relocation             false "ALTER TABLE users SET TABLESPACE pg_default"
    dry_run refuse         unsupported-operation  false "ALTER TABLE users ENABLE ROW LEVEL SECURITY"
}

run_diff() {
    heading "Declarative front door: routed convergence plans (read-only)"
    diff_plan desired/users_v2.sql 2 "ADD COLUMN bio"
    diff_plan desired/widgets_v1.sql 2 "CREATE TABLE"
}

run_offline() {
    heading "Offline commands (no database)"
    local out status

    # lint gates: error-severity findings exit non-zero.
    step "lint risky.sql"
    status=0
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" lint --json risky.sql) || status=$?
        if [ "$status" = 0 ]; then
            fail "lint risky.sql: expected a non-zero exit for error findings"
        fi
        if [ "$(jq -r '.errors' <<<"$out")" = 0 ]; then
            fail "lint risky.sql: expected error findings"
        fi
    else
        "$PGS" lint risky.sql || echo "(exit $? — error findings gate, by design)"
    fi

    # suggest advises: always exits zero.
    step "suggest risky.sql"
    status=0
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" suggest --json risky.sql) || status=$?
        assert_eq "suggest exit" 0 "$status"
        if [ "$(jq -r '.suggestions | length' <<<"$out")" = 0 ]; then
            fail "suggest risky.sql: expected suggestions"
        fi
    else
        "$PGS" suggest risky.sql || echo "(exit $?)"
    fi

    # fmt refuses commented input (the deparser would silently drop the
    # comments), so the tour feeds it a comment-free statement.
    step "fmt (stdin)"
    status=0
    out=$(echo "create table users (id bigint generated by default as identity primary key, name varchar(50));" | "$PGS" fmt) || status=$?
    if [ "$CHECK" = 1 ]; then
        assert_eq "fmt exit" 0 "$status"
        if [ -z "$out" ]; then
            fail "fmt: expected canonicalized output"
        fi
    else
        printf '%s\n' "$out"
    fi
}

run_exec() {
    heading "Real executions against the seeded tables (make demo reseeds each run)"
    execute_native "ALTER TABLE users ADD COLUMN bio text"
    execute_native "CREATE INDEX idx_users_email ON users (email)"
    execute_native "ALTER TABLE users ALTER COLUMN email SET NOT NULL"
    execute_refused backend-unavailable "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
}

case "$section" in
dryrun)  run_dryrun ;;
diff)    run_diff ;;
offline) run_offline ;;
exec)    run_exec ;;
all)     run_dryrun; run_diff; run_offline; run_exec ;;
*)
    echo "usage: tour.sh [dryrun|diff|offline|exec|all]" >&2
    exit 64
    ;;
esac

if [ "$CHECK" = 1 ]; then
    if [ "$failures" -gt 0 ]; then
        printf '\ndemo tour: %d check(s) failed\n' "$failures" >&2
        exit 1
    fi
    printf '\ndemo tour: all checks passed\n'
fi
