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
reset() {
	rm -f "$FAKE_DIR/api.err"
	echo '[]' >"$FAKE_DIR/issues.json"
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
lacks "comment"
lacks "close"
report

reset
echo '[{"number":9},{"number":7}]' >"$FAKE_DIR/issues.json"
jobs failure success success failure success
run "a failure with an open issue comments on the oldest one" 0 failure
has "comment"
has "7"
has "- Majors that did not pass: 14 17"
lacks "9"
lacks "--title"
report

reset
echo '[{"number":7}]' >"$FAKE_DIR/issues.json"
run "a pass with an open issue closes it with the passing run" 0 success
has "close"
has "7"
has "The scheduled run on $(date -u +%Y-%m-%d) passed on every major: $run_url"
lacks "--title"
report

reset
run "a pass with no open issue opens, comments on, and closes nothing" 0 success
lacks "--title"
lacks "comment"
lacks "close"
report

reset
echo '[{"number":7}]' >"$FAKE_DIR/issues.json"
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
