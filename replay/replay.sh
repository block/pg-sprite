#!/usr/bin/env bash
#
# Replay a project's schema-change corpus through pg-sprite, from scratch.
#
# Resets the harness database to the project baseline, then walks
# replay/<project>/assessment.tsv in order. Every assessed step (execute /
# refuse:<reason>:<class>[:<owner>]) runs through `pg-sprite migrate --json`
# against the live database — real execution, not dry-run. Refused steps are
# then applied via psql so state keeps advancing; psql steps (out-of-scope
# content) are applied via psql in one transaction and never assessed.
#
# The run fails (non-zero exit) on any verdict mismatch: an assessed step
# that does not produce exactly its expected outcome — including a refusal
# with the wrong reason, class, or owner — is a failure, not a pass.
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

# Refuse expectations are refuse:<reason>:<class>[:<owner>], parsed from the
# right: class and owner are closed, disjoint vocabularies, so the last
# token is an owner exactly when it names one, and the token before the
# class is the reason (which may itself contain colons).
is_owner() {
    case "$1" in
    data-change-runner|declarative-front-door|direct-operator|provisioning) return 0 ;;
    esac
    return 1
}

# A manifest pins verdicts the corpus itself determines. Two classes describe
# the run rather than the operation (docs/refusal-classes.md) and so are never
# a legitimate expectation: invariant-violation reports a pg-sprite defect,
# and environmental depends on the run's budgets, privileges, and server —
# a row pinning either would go green over a failure the corpus does not
# control.
parse_refuse_expectation() {
    local expectation="${1#refuse:}" where="$2"
    want_owner=""
    if is_owner "${expectation##*:}"; then
        want_owner="${expectation##*:}"
        expectation="${expectation%:*}"
    fi
    case "$expectation" in
    *:*) ;;
    *) die "refusal expectation must pin a class (refuse:<reason>:<class>) in $where" ;;
    esac
    want_reason="${expectation%:*}"
    want_class="${expectation##*:}"
    case "$want_class" in
    capability-boundary|by-design)
        [ -z "$want_owner" ] \
            || die "class $want_class carries no owner (RF-7) in $where" ;;
    no-online-safety-problem)
        [ -n "$want_owner" ] \
            || die "class $want_class must name its owner" \
                "(refuse:<reason>:<class>:<owner>, RF-7) in $where" ;;
    environmental|invariant-violation)
        die "class $want_class describes the run, not the operation, and is never" \
            "an expected verdict (docs/refusal-classes.md) in $where" ;;
    *) die "unknown pinned refusal class '$want_class' in $where" ;;
    esac
}

# parse_row validates one manifest row and, for a refuse row, sets
# want_reason / want_class / want_owner. Called once over the whole manifest
# before the harness is touched — a malformed pin must fail before anything
# runs, not on the row it sits on — and again per row during the walk.
parse_row() {
    local migration="$1" range="$2" expected="$3" extra="$4"
    local where="$MANIFEST: $migration $range"
    want_reason=""; want_class=""; want_owner=""

    # The class rides inside the refuse expectation; a refuse row with a bare
    # trailing token is the shape of a manifest written before that move, so
    # name the move rather than the symptom.
    if [ -n "$extra" ]; then
        case "$expected" in
        refuse:*)
            die "unexpected fourth column '$extra' in $where — the class belongs" \
                "inside the expectation: refuse:<reason>:<class>[:<owner>]" ;;
        *) die "unexpected fourth column '$extra' in $where" ;;
        esac
    fi

    case "$expected" in
    execute|psql) ;;
    refuse:*) parse_refuse_expectation "$expected" "$where" ;;
    *) die "unknown expectation '$expected' in $where" ;;
    esac
}

while read -r migration range expected extra; do
    case "$migration" in ''|\#*) continue ;; esac
    parse_row "$migration" "$range" "$expected" "$extra"
done <"$MANIFEST"

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
    parse_row "$migration" "$range" "$expected" "$extra"

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
        # Buckets count the engine's verdict, not the pin — the class it
        # emitted is the finding about the workload. A mismatched row is
        # still counted under what the engine said; the summary header says
        # so whenever failures is non-zero.
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
        # Owner is asserted on every row: RF-7 makes it present exactly when
        # the class is no-online-safety-problem, so the pin (or its absence)
        # is well-defined without a separate column.
        if [ "$status" -eq 2 ] && [ "$outcome" = "refused" ] \
            && [ "$reason" = "$want_reason" ] && [ "$actual_class" = "$want_class" ] \
            && [ "$owner" = "$want_owner" ]; then
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
# The buckets tally engine verdicts, so they are a finding about the workload
# only when every verdict matched its pin; otherwise say what they are.
if [ "$failures" -eq 0 ]; then
    echo "== bucket summary"
else
    echo "== bucket summary (engine verdicts as emitted; $failures row(s) mismatched" \
        "their pin — see FAIL rows above)"
fi
# Labels are a fixed set but of uneven length; size the column from them so
# the loudest label cannot push its own count out of line.
summary_labels=(
    "executed natively by pg-sprite (T1)"
    "refusal: capability boundary (T2)"
    "refusal: no online-safety problem"
    "refusal: by design (safer form exists)"
    "refusal: environmental"
    "refusal: INVARIANT VIOLATION (REPORT PG-SPRITE DEFECT)"
    "out-of-scope content, psql only (T3)"
    "mismatches"
)
label_width=0
for l in "${summary_labels[@]}"; do
    [ "${#l}" -gt "$label_width" ] && label_width=${#l}
done
summary_line() { printf '%-*s %s\n' "$label_width" "$1" "$2"; }
summary_line "${summary_labels[0]}" "$count_executed"
summary_line "${summary_labels[1]}" "$count_capability_boundary"
if [ "$count_capability_boundary" -gt 0 ]; then
    printf '%s\n' "${capability_boundary_reasons[@]}" | sort | uniq -c | sort -rn \
        | while read -r n reason; do
            printf '  %-*s %s\n' "$((label_width - 2))" "$reason" "$n"
        done
fi
summary_line "${summary_labels[2]}" "$count_no_online_safety_problem"
summary_line "${summary_labels[3]}" "$count_by_design"
summary_line "${summary_labels[4]}" "$count_environmental"
summary_line "${summary_labels[5]}" "$count_invariant_violation"
summary_line "${summary_labels[6]}" "$count_psql"
summary_line "${summary_labels[7]}" "$failures"

[ "$failures" -eq 0 ] || exit 1
echo
echo "replay complete: every assessed statement matched its expected verdict"
