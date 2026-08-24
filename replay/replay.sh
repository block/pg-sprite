#!/usr/bin/env bash
#
# Replay a project's schema-change corpus through pg-sprite, from scratch.
#
# Resets the harness database to the project baseline, then walks
# replay/<project>/assessment.tsv in order. Every assessed step (execute /
# refuse:<reason>) runs through `pg-sprite migrate --json` against the live
# database — real execution, not dry-run. Refused steps are then applied via
# psql so state keeps advancing; psql steps (out-of-scope content) are
# applied via psql in one transaction and never assessed.
#
# The run fails (non-zero exit) on any verdict mismatch: an assessed step
# that does not produce exactly its expected outcome — including a refusal
# with the wrong reason — is a failure, not a pass.
#
#   make replay [REPLAY_PROJECT=<project>]   # fetch + harness + full replay
#   ./replay.sh <project>                     # replay directly (starts or
#                                             # resets the harness itself)
set -uo pipefail

REPLAY_DIR="$(cd "$(dirname "$0")" && pwd)"
. "${REPLAY_DIR}/common.sh"
load_project "${1:-}"

PGS="${PGS:-${REPLAY_DIR}/../bin/pg-sprite}"
MANIFEST="${PROJECT_DIR}/assessment.tsv"

command -v python3 >/dev/null 2>&1 || die "python3 is required to parse verdicts"
[ -x "$PGS" ] || die "pg-sprite binary not found at $PGS — run make build first"
[ -s "$MANIFEST" ] || die "missing $MANIFEST"

DSN="$("${REPLAY_DIR}/harness.sh" "$PROJECT" dsn)"

# One replay step's SQL, extracted from the pinned corpus by line range.
extract() {
    local file="$1" start="$2" end="$3"
    sed -n "${start},${end}p" "$file"
}

# Apply SQL (stdin) via psql in a single transaction, mirroring the original
# per-migration transaction envelope for content pg-sprite does not run.
psql_apply() {
    "${REPLAY_DIR}/harness.sh" "$PROJECT" psql -q -X -v ON_ERROR_STOP=1 --single-transaction >/dev/null
}

# First statement line of a SQL block, squeezed, for the results table.
preview() {
    printf '%s' "$1" | grep -vE '^[[:space:]]*(--|$)' | head -1 \
        | sed -E 's/^[[:space:]]+//; s/[[:space:]]+/ /g' | cut -c1-56
}

# Either path ends at the pristine baseline: a fresh container applies it on
# the way up; an existing one is dropped back to it. A failed baseline must
# stop the run — replaying against a partially-applied starting state would
# produce a results table that looks legitimate but assesses the wrong world.
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
    "${REPLAY_DIR}/harness.sh" "$PROJECT" reset || die "harness reset failed"
else
    "${REPLAY_DIR}/harness.sh" "$PROJECT" up || die "harness up failed"
fi

rows=()
failures=0
count_executed=0
count_psql=0
declare -a refuse_reasons=()

while read -r migration range expected; do
    case "$migration" in ''|\#*) continue ;; esac

    file=$(ls "${PROJECT_DIR}/corpus/${migration}_"*.sql 2>/dev/null) \
        || die "no corpus file for migration $migration"
    start="${range%-*}"; end="${range#*-}"
    sql="$(extract "$file" "$start" "$end")"
    [ -n "$sql" ] || die "empty extraction: $migration $range"
    label="$(preview "$sql")"

    if [ "$expected" = "psql" ]; then
        if printf '%s\n' "$sql" | psql_apply; then
            rows+=("$migration|$range|psql|applied|PASS|$label")
            count_psql=$((count_psql + 1))
        else
            rows+=("$migration|$range|psql|apply-error|FAIL|$label")
            failures=$((failures + 1))
        fi
        continue
    fi

    out="$("$PGS" migrate --url "$DSN" --json --alter "$sql" 2>/dev/null)"
    status=$?
    actual="$(printf '%s' "$out" | python3 -c '
import json, sys
try:
    v = json.load(sys.stdin)
except Exception:
    print("unparseable")
    raise SystemExit
outcome = v.get("outcome", "unknown")
reason = v.get("reason", "")
print(f"{outcome}:{reason}" if reason else outcome)
')"

    case "$expected" in
    execute)
        if [ "$status" -eq 0 ] && [ "$actual" = "executed-natively" ]; then
            rows+=("$migration|$range|execute|$actual|PASS|$label")
            count_executed=$((count_executed + 1))
        else
            rows+=("$migration|$range|execute|$actual (exit $status)|FAIL|$label")
            failures=$((failures + 1))
            # Advance state via psql so later steps stay meaningful, but only
            # when pg-sprite refused (nothing ran); a failed execution may
            # have committed a prefix and needs manual inspection instead.
            if [ "$status" -eq 2 ]; then
                printf '%s\n' "$sql" | psql_apply \
                    || die "advance after unexpected refusal failed: $migration $range"
            fi
        fi
        ;;
    refuse:*)
        want_reason="${expected#refuse:}"
        if [ "$status" -eq 2 ] && [ "$actual" = "refused:${want_reason}" ]; then
            rows+=("$migration|$range|$expected|$actual|PASS|$label")
            refuse_reasons+=("$want_reason")
        else
            rows+=("$migration|$range|$expected|$actual (exit $status)|FAIL|$label")
            failures=$((failures + 1))
        fi
        # A refusal (exit 2) executed nothing, so the corpus statement itself
        # must still land for the remaining history to replay against true
        # state. Advance only on a true refusal — mirroring the execute
        # branch, an exit-1 failed execution may have committed a prefix
        # that must not be blindly re-applied.
        if [ "$status" -eq 2 ]; then
            printf '%s\n' "$sql" | psql_apply \
                || die "psql advance failed: $migration $range"
        fi
        ;;
    *)
        die "unknown expectation '$expected' in $MANIFEST: $migration $range"
        ;;
    esac
done <"$MANIFEST"

echo
echo "== per-statement results"
printf '%-5s %-9s %-38s %-42s %-5s %s\n' MIG LINES EXPECTED ACTUAL RES STATEMENT
for row in "${rows[@]}"; do
    IFS='|' read -r m r e a res l <<<"$row"
    printf '%-5s %-9s %-38s %-42s %-5s %s\n' "$m" "$r" "$e" "$a" "$res" "$l"
done

echo
echo "== bucket summary"
printf '%-52s %s\n' "executed natively by pg-sprite (T1)" "$count_executed"
total_refused=${#refuse_reasons[@]}
printf '%-52s %s\n' "typed refusal, advanced via psql (T2 boundary)" "$total_refused"
if [ "$total_refused" -gt 0 ]; then
    printf '%s\n' "${refuse_reasons[@]}" | sort | uniq -c | sort -rn \
        | while read -r n reason; do
            printf '  %-50s %s\n' "$reason" "$n"
        done
fi
printf '%-52s %s\n' "out-of-scope content, psql only (T3)" "$count_psql"
printf '%-52s %s\n' "mismatches" "$failures"

[ "$failures" -eq 0 ] || exit 1
echo
echo "replay complete: every assessed statement matched its expected verdict"
