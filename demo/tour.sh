#!/usr/bin/env bash
#
# A runnable tour of the pg-sprite CLI: walk one statement through every
# planner route and reason, print the declarative diff plans, run the
# offline commands, export and verify a declarative baseline, and finish by executing real schema changes — including
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

# The tour cds into demo/ below, so anchor a relative binary path first
# (a bare command name still resolves via PATH).
if [[ "$PGS" == */* && "$PGS" != /* ]]; then
    PGS="$(pwd)/$PGS"
fi

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

# dry_run route reason destructive disposition sql
#
# Demo mode prints the routed plan. Check mode asserts the statement's
# route, first decision reason, destructive flag, and the report's
# disposition from the JSON plan report. Dry-run never writes, so these
# rows can rerun in any order.
#
# Exit code: an execute-disposition plan exits 0; any plan that would not
# execute (rewrite-required, unavailable, refuse) exits with the refusal
# code 2 — the binary under test is always this tree's build, so the
# contract is pinned exactly.
dry_run() {
    local route="$1" reason="$2" destructive="$3" disposition="$4" sql="$5" out status=0
    step "$sql"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" migrate --url "$PG_DSN" --dry-run --json --alter "$sql") || status=$?
        if [ "$disposition" = execute ]; then
            assert_eq "dry-run exit of [$sql]" 0 "$status"
        else
            assert_eq "dry-run refusal exit of [$sql]" 2 "$status"
        fi
        assert_eq "plan format_version of [$sql]" 2 "$(jq -r '.format_version' <<<"$out")"
        assert_eq "disposition of [$sql]" "$disposition" "$(jq -r '.disposition' <<<"$out")"
        assert_eq "route of [$sql]" "$route" "$(jq -r '.statements[0].route' <<<"$out")"
        assert_eq "reason of [$sql]" "$reason" "$(jq -r '.statements[0].decisions[0].reason' <<<"$out")"
        assert_eq "destructive of [$sql]" "$destructive" "$(jq -r '.statements[0].destructive' <<<"$out")"
    else
        "$PGS" migrate --url "$PG_DSN" --dry-run --alter "$sql" || echo "(exit $?)"
    fi
}

# execute_native steps fragment sql
#
# Runs the schema change for real and requires the executed-natively
# verdict plus the shape of what actually ran. The verdict's executed_sql
# is present only when the engine substituted a safer sequence, so
# steps=0 pins an as-written run and steps>0 pins the substitution — the
# step count and a distinguishing fragment (CONCURRENTLY, NOT VALID),
# never literal SQL, because the exact steps are the engine's to choose.
# The step shape is stable here because the demo compose pins one
# PostgreSQL major.
execute_native() {
    local steps="$1" fragment="$2" sql="$3" out status=0
    step "execute: $sql"
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" migrate --url "$PG_DSN" --json --alter "$sql") || status=$?
        assert_eq "exit of [$sql]" 0 "$status"
        assert_eq "outcome of [$sql]" "executed-natively" "$(jq -r '.outcome' <<<"$out")"
        assert_eq "substituted steps of [$sql]" "$steps" "$(jq -r '.executed_sql | length' <<<"$out")"
        if [ -n "$fragment" ]; then
            case "$(jq -r '.executed_sql | join("; ")' <<<"$out")" in
            *"$fragment"*) ;;
            *) fail "executed_sql of [$sql]: expected it to contain '$fragment'" ;;
            esac
        fi
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
        assert_eq "diff format_version of [$desired]" 2 "$(jq -r '.format_version' <<<"$out")"
        assert_eq "statement count of [$desired]" "$count" "$(jq -r '.statements | length' <<<"$out")"
        case "$(jq -r '.statements[0].sql' <<<"$out")" in
        *"$fragment"*) ;;
        *) fail "first statement of [$desired]: expected it to contain '$fragment'" ;;
        esac
    else
        "$PGS" diff --url "$PG_DSN" --desired "$desired" --schema public || echo "(exit $?)"
    fi
}

run_pull() {
    heading "Existing database onboarding: export a baseline and prove zero diff"
    local out_dir file_count desired out status
    out_dir=$(mktemp -d "${TMPDIR:-/tmp}/pg-sprite-pull.XXXXXX")

    step "pull --schema public --out $out_dir"
    status=0
    if [ "$CHECK" = 1 ]; then
        "$PGS" pull --url "$PG_DSN" --schema public --out "$out_dir" >/dev/null || status=$?
        assert_eq "pull exit" 0 "$status"
        file_count=$(find "$out_dir" -maxdepth 1 -type f -name '*.sql' | wc -l | tr -d ' ')
        assert_eq "pulled file count" 2 "$file_count"
    else
        "$PGS" pull --url "$PG_DSN" --schema public --out "$out_dir" || echo "(exit $?)"
    fi

    for desired in "$out_dir"/*.sql; do
        step "zero-diff proof: $desired"
        status=0
        if [ "$CHECK" = 1 ]; then
            out=$("$PGS" diff --url "$PG_DSN" --schema public --desired "$desired" --json) || status=$?
            assert_eq "diff exit of [$desired]" 0 "$status"
            assert_eq "diff format_version of [$desired]" 2 "$(jq -r '.format_version' <<<"$out")"
            assert_eq "diff disposition of [$desired]" execute "$(jq -r '.disposition' <<<"$out")"
            assert_eq "zero diff of [$desired]" 0 "$(jq -r '.statements | length' <<<"$out")"
        else
            "$PGS" diff --url "$PG_DSN" --schema public --desired "$desired" || echo "(exit $?)"
        fi
    done
    rm -rf "$out_dir"
}

run_dryrun() {
    heading "Classification: one statement per route and reason (dry-run, no writes)"
    #       route          reason                 destructive disposition
    dry_run native         metadata-only          false execute     "ALTER TABLE users ADD COLUMN bio text"
    dry_run native         fast-default           false execute     "ALTER TABLE users ADD COLUMN tier text DEFAULT 'free'"
    dry_run native         binary-coercible       false execute     "ALTER TABLE users ALTER COLUMN name TYPE varchar(100)"
    dry_run native         safer-idiom            false execute     "ALTER TABLE users ALTER COLUMN email SET NOT NULL"
    dry_run native         safer-idiom            false execute     "CREATE INDEX idx_users_email ON users (email)"
    dry_run native         online-idiom           false execute     "CREATE INDEX CONCURRENTLY idx_users_email ON users (email)"
    dry_run native         safer-idiom            false execute     "ALTER TABLE users ADD CONSTRAINT users_name_nonempty CHECK (char_length(name) > 0)"
    dry_run native         safer-idiom            false execute     "ALTER TABLE users ADD CONSTRAINT users_email_uniq UNIQUE (email)"
    dry_run native         safer-idiom            false rewrite-required "ALTER TABLE users ADD COLUMN nick text UNIQUE"
    dry_run copy-and-swap  type-rewrite           false unavailable "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
    dry_run copy-and-swap  volatile-default       false unavailable "ALTER TABLE users ADD COLUMN joined timestamptz DEFAULT now()"
    dry_run native         app-breaking-rename    false execute     "ALTER TABLE users RENAME COLUMN name TO full_name"
    dry_run native         metadata-only          true  execute     "ALTER TABLE users DROP COLUMN status"
    dry_run copy-and-swap  relocation             false unavailable "ALTER TABLE users SET TABLESPACE pg_default"
    dry_run refuse         unsupported-operation  false refuse      "ALTER TABLE users ENABLE ROW LEVEL SECURITY"
}

run_diff() {
    heading "Declarative front door: routed convergence plans (read-only)"
    diff_plan desired/users_v2.sql 2 "ADD COLUMN bio"
    diff_plan desired/widgets_v1.sql 2 "CREATE TABLE"
}

run_offline() {
    heading "Offline commands (no database)"
    local out status

    # lint gates: risky.sql carries exactly one error-severity finding (the
    # refused operation), so the exit code and count are knowable — a vague
    # "something failed" check would also pass on a missing binary.
    step "lint risky.sql"
    status=0
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" lint --json risky.sql) || status=$?
        assert_eq "lint exit" 1 "$status"
        assert_eq "lint format_version" 1 "$(jq -r '.format_version' <<<"$out")"
        assert_eq "lint errors" 1 "$(jq -r '.errors' <<<"$out")"
    else
        "$PGS" lint risky.sql || echo "(exit $? — error findings gate, by design)"
    fi

    # suggest advises: always exits zero; the fixture is fixed, so the
    # suggestion count is knowable.
    step "suggest risky.sql"
    status=0
    if [ "$CHECK" = 1 ]; then
        out=$("$PGS" suggest --json risky.sql) || status=$?
        assert_eq "suggest exit" 0 "$status"
        assert_eq "suggest format_version" 2 "$(jq -r '.format_version' <<<"$out")"
        assert_eq "suggest count" 2 "$(jq -r '.suggestions | length' <<<"$out")"
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
        if [ "$status" != 0 ]; then
            echo "(exit $status)"
        fi
    fi
}

run_exec() {
    heading "Real executions against the seeded tables (make demo reseeds each run)"
    #              steps fragment
    execute_native 0     ""            "ALTER TABLE users ADD COLUMN bio text"
    execute_native 1     CONCURRENTLY  "CREATE INDEX idx_users_email ON users (email)"
    execute_native 4     "NOT VALID"   "ALTER TABLE users ALTER COLUMN email SET NOT NULL"
    execute_refused backend-unavailable "ALTER TABLE orders ALTER COLUMN user_id TYPE bigint"
}

case "$section" in
dryrun)  run_dryrun ;;
diff)    run_diff ;;
pull)    run_pull ;;
offline) run_offline ;;
exec)    run_exec ;;
all)     run_dryrun; run_diff; run_pull; run_offline; run_exec ;;
*)
    echo "usage: tour.sh [dryrun|diff|pull|offline|exec|all]" >&2
    exit 64
    ;;
esac

if [ "$CHECK" = 1 ]; then
    if [ "$failures" -gt 0 ]; then
        printf '\ndemo tour: %d check(s) failed\n' "$failures" >&2
        exit 1
    fi
    printf '\ndemo tour: all checks passed\n'
elif [ "$section" = all ]; then
    heading "Tour complete"
    echo "Every statement above was classified before anything ran. The"
    echo "executions committed online — where the submitted form would have"
    echo "blocked, pg-sprite ran the safer native sequence instead — and the"
    echo "changes it cannot yet do safely were refused before they could hurt."
fi
