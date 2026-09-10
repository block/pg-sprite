#!/usr/bin/env bash
#
# Replay a project's schema-change corpus through pg-sprite, from scratch.
#
# Resets the harness database to the project baseline, then walks
# replay/<project>/assessment.tsv in order. Every assessed step (execute /
# refuse:<reason>:<class>) runs through `pg-sprite migrate --json` against the live
# database — real execution, not dry-run. Refused steps are then applied via
# psql so state keeps advancing; psql steps (out-of-scope content) are
# applied via psql in one transaction and never assessed.
#
# The run fails (non-zero exit) on any verdict mismatch: an assessed step
# that does not produce exactly its expected outcome — including a refusal
# with the wrong reason or class — is a failure, not a pass.
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
count_capability_boundary=0
count_no_online_safety_problem=0
count_by_design=0
count_environmental=0
count_invariant_violation=0
declare -a capability_boundary_reasons=()

while read -r migration range expected extra; do
    case "$migration" in ''|\#*) continue ;; esac

    [ -z "$extra" ] || die "unexpected fourth column in $MANIFEST: $migration $range"

    case "$expected" in
    refuse:*:*)
        expectation="${expected#refuse:}"
        want_reason="${expectation%:*}"
        want_class="${expectation##*:}"
        case "$want_class" in
        capability-boundary|no-online-safety-problem|by-design|environmental|invariant-violation) ;;
        *) die "unknown pinned refusal class '$want_class' in $MANIFEST: $migration $range" ;;
        esac
        ;;
    refuse:*) die "refusal expectation must pin a class in $MANIFEST: $migration $range" ;;
    esac

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
    verdict_fields="$(printf '%s' "$out" | python3 -c '
import json, sys
try:
    v = json.load(sys.stdin)
except Exception:
    print("unparseable\t\t\t")
    raise SystemExit
print("\t".join(str(v.get(k, "")) for k in ("outcome", "reason", "class", "owner")))
')"
    IFS=$'\t' read -r outcome reason actual_class owner <<<"$verdict_fields"
    actual="$outcome"
    [ -n "$reason" ] && actual="${actual}:${reason}"
    [ -n "$actual_class" ] && actual="${actual}:${actual_class}"
    [ -n "$owner" ] && actual="${actual} owner:${owner}"

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
        if [ "$status" -eq 2 ]; then
            case "$actual_class" in
            capability-boundary)
                count_capability_boundary=$((count_capability_boundary + 1))
                capability_boundary_reasons+=("$reason")
                ;;
            no-online-safety-problem) count_no_online_safety_problem=$((count_no_online_safety_problem + 1)) ;;
            by-design) count_by_design=$((count_by_design + 1)) ;;
            environmental) count_environmental=$((count_environmental + 1)) ;;
            invariant-violation) count_invariant_violation=$((count_invariant_violation + 1)) ;;
            esac
        fi
        if [ "$status" -eq 2 ] && [ "$outcome" = "refused" ] \
            && [ "$reason" = "$want_reason" ] && [ "$actual_class" = "$want_class" ]; then
            rows+=("$migration|$range|$expected|$actual|PASS|$label")
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
# EXPECTED and ACTUAL carry reason and class segments of unbounded length, so
# size those columns from the rows being printed instead of a fixed width
# that a long refusal would overflow and misalign.
expected_width=8
actual_width=6
for row in "${rows[@]}"; do
    IFS='|' read -r _ _ e a _ _ <<<"$row"
    [ "${#e}" -gt "$expected_width" ] && expected_width=${#e}
    [ "${#a}" -gt "$actual_width" ] && actual_width=${#a}
done
printf '%-5s %-9s %-*s %-*s %-5s %s\n' MIG LINES \
    "$expected_width" EXPECTED "$actual_width" ACTUAL RES STATEMENT
for row in "${rows[@]}"; do
    IFS='|' read -r m r e a res l <<<"$row"
    printf '%-5s %-9s %-*s %-*s %-5s %s\n' "$m" "$r" \
        "$expected_width" "$e" "$actual_width" "$a" "$res" "$l"
done

echo
echo "== bucket summary"
printf '%-52s %s\n' "executed natively by pg-sprite (T1)" "$count_executed"
printf '%-52s %s\n' "refusal: capability boundary (T2)" "$count_capability_boundary"
if [ "$count_capability_boundary" -gt 0 ]; then
    printf '%s\n' "${capability_boundary_reasons[@]}" | sort | uniq -c | sort -rn \
        | while read -r n reason; do
            printf '  %-50s %s\n' "$reason" "$n"
        done
fi
printf '%-52s %s\n' "refusal: no online-safety problem" "$count_no_online_safety_problem"
printf '%-52s %s\n' "refusal: by design (safer form exists)" "$count_by_design"
printf '%-52s %s\n' "refusal: environmental" "$count_environmental"
printf '%-52s %s\n' "refusal: INVARIANT VIOLATION (REPORT PG-SPRITE DEFECT)" "$count_invariant_violation"
printf '%-52s %s\n' "out-of-scope content, psql only (T3)" "$count_psql"
printf '%-52s %s\n' "mismatches" "$failures"

[ "$failures" -eq 0 ] || exit 1
echo
echo "replay complete: every assessed statement matched its expected verdict"
