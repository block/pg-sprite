#!/bin/bash
#
# Drive scripts/slot-release-proof-issue.sh through each outcome against the
# stand-in gh in scripts/testdata/slot-release-proof-issue, with no network
# and no GitHub token. Each case writes the open issues and the run's jobs as
# GitHub returns them, runs the script, and checks its exit code and the gh
# calls it made. Needs jq, as the stand-in applies the script's --jq filters.
#
# Usage: scripts/slot-release-proof-issue_test.sh

set -euo pipefail

scripts="$(cd "$(dirname "$0")" && pwd)"
script="$scripts/slot-release-proof-issue.sh"
standin="$scripts/testdata/slot-release-proof-issue"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export FAKE_DIR="$work"
export GH_REPO=acme/sprite RUN_ID=123 RUN_ATTEMPT=2 GITHUB_SERVER_URL=https://github.example
run_url="https://github.example/acme/sprite/actions/runs/123"

failures=0
case_failed=0
fail() {
	echo "    $1" >&2
	case_failed=1
}
# run NAME WANT_EXIT RESULT...: run the script with the fixtures in place.
run() {
	local name="$1" want="$2"
	shift 2
	current="$name"
	case_failed=0
	: >"$FAKE_DIR/calls"
	: >"$FAKE_DIR/lines"
	local got=0
	PATH="$standin:$PATH" "$script" "$@" >"$work/out" 2>&1 || got=$?
	if [ "$got" -ne "$want" ]; then
		fail "exit $got, want $want"
		sed 's/^/    /' "$work/out" >&2
	fi
}
# has LINE / lacks LINE: whether some gh argument line is exactly LINE.
has() { grep -Fxq -- "$1" "$FAKE_DIR/calls" || fail "no gh argument line: $1"; }
lacks() { if grep -Fxq -- "$1" "$FAKE_DIR/calls"; then fail "unexpected gh argument line: $1"; fi; }
# has_call TEXT / lacks_call TEXT: whether some single gh call's space-joined
# arguments contain TEXT, so flags that must travel together are pinned so.
has_call() { grep -Fq -- "$1" "$FAKE_DIR/lines" || fail "no gh call containing: $1"; }
lacks_call() { if grep -Fq -- "$1" "$FAKE_DIR/lines"; then fail "unexpected gh call containing: $1"; fi; }
report() {
	if [ "$case_failed" -ne 0 ]; then
		echo "FAIL $current" >&2
		failures=$((failures + 1))
	else
		echo "ok   $current"
	fi
}
# jobs CONCLUSION...: the run's jobs, one proof job per major 14 -> 18 with the
# given conclusions, plus the keepalive job and the still-running report job.
jobs() {
	local majors=(14 15 16 17 18) entries="" i
	for i in "${!majors[@]}"; do
		entries+="{\"name\":\"slot release proof (PostgreSQL ${majors[$i]})\",\"conclusion\":\"$1\"},"
		shift
	done
	printf '{"total_count":7,"jobs":[%s{"name":"keepalive","conclusion":"success"},{"name":"report","conclusion":null}]}\n' "$entries" >"$FAKE_DIR/jobs.json"
}
# issues NUMBER...: the open labelled issues the job opened, with its title.
issues() {
	local entries="" n
	for n in "$@"; do
		entries+="{\"number\":$n,\"title\":\"Scheduled slot release proof failed\"},"
	done
	printf '[%s]\n' "${entries%,}" >"$FAKE_DIR/issues.json"
}
# latest ID: the newest scheduled run of the workflow.
latest() { printf '{"total_count":1,"workflow_runs":[{"id":%s}]}\n' "$1" >"$FAKE_DIR/runs.json"; }
reset() {
	rm -f "$FAKE_DIR/api.err"
	echo '[]' >"$FAKE_DIR/issues.json"
	latest 123
	jobs success success success success success
}

reset
jobs success failure success success success
run "a failure with no open issue opens one naming the run and the failed major" 0 failure
has "label"
has "slot-release-proof"
has "--title"
has "Scheduled slot release proof failed"
has "- Run: $run_url"
has "- Majors that did not pass: 15"
has_call "issue list --label slot-release-proof --state open"
has_call "label create slot-release-proof"
has_call "issue create --title Scheduled slot release proof failed --label slot-release-proof --body"
lacks "comment"
lacks "close"
report

reset
run "a failure with every per-major job green says so" 0 failure
has "- Majors that did not pass: none of the per-major jobs failed (result: failure)"
report

reset
echo '[{"number":5,"title":"Track slot release cadence on PostgreSQL 18"}]' >"$FAKE_DIR/issues.json"
run "a pass leaves a hand-written issue that only shares the label open" 0 success
lacks "close"
lacks "5"
report

reset
echo '[{"number":5,"title":"Track slot release cadence on PostgreSQL 18"}]' >"$FAKE_DIR/issues.json"
jobs success failure success success success
run "a failure opens its own issue beside a hand-written one with the label" 0 failure
has "--title"
lacks "comment"
report

reset
issues 7
latest 130
run "a re-run of an older Monday that passes does not close a newer failure's issue" 0 success
lacks "close"
lacks_call "label create"
report

reset
latest 130
jobs success failure success success success
run "a re-run of an older Monday that fails opens nothing" 0 failure
lacks "--title"
lacks "comment"
report

reset
issues 9 7
jobs failure success success failure success
run "a failure with an open issue comments on the oldest one" 0 failure
has "comment"
has "7"
has "- Majors that did not pass: 14 17"
lacks "9"
lacks "--title"
report

reset
issues 7
run "a pass with an open issue closes it with the passing run" 0 success
has "close"
has "7"
has "The scheduled run on $(date -u +%Y-%m-%d) passed on every major: $run_url"
lacks "--title"
lacks_call "label create"
report

reset
run "a pass with no open issue writes nothing" 0 success
lacks "--title"
lacks "comment"
lacks "close"
lacks_call "label create"
report

reset
issues 7
jobs cancelled cancelled cancelled cancelled cancelled
run "a cancelled proof is reported and does not close the issue" 0 cancelled
has "comment"
has "The scheduled Slot Release Proof on $(date -u +%Y-%m-%d) did not pass (result: cancelled)."
has "- Majors that did not pass: 14 15 16 17 18"
lacks "close"
report

reset
echo "HTTP 403: Resource not accessible by integration" >"$FAKE_DIR/api.err"
run "a failure whose jobs cannot be listed still opens the issue" 0 failure
has "--title"
has "- Majors that did not pass: unknown: listing the run's jobs failed; open the run to see them"
report

reset
run "a missing result is a usage error and calls no gh" 2
if [ -s "$FAKE_DIR/calls" ]; then fail "gh was called"; fi
report

if [ "$failures" -gt 0 ]; then
	echo "$failures slot-release-proof-issue cases failed" >&2
	exit 1
fi
