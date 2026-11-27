#!/bin/bash
#
# Keep one GitHub issue in step with the scheduled Slot Release Proof.
#
# GitHub opens no issue when a scheduled workflow fails; it only emails whoever
# last edited the schedule. The workflow's report job runs this after every
# scheduled run with the proof matrix's overall result:
#
#   success   close the open issue, if there is one, with a comment linking
#             the passing run.
#   anything  open the issue, or comment on the open one, with the run's link,
#   else      the date, and the majors whose job did not pass. A cancelled or
#             skipped proof proved nothing that week, so it is reported too,
#             and it never closes the issue.
#
# The open issue is the one carrying the slot-release-proof label, so a
# failure that lasts several weeks is one issue with a comment per run. The
# label is created when it is missing.
#
# Usage: scripts/slot-release-proof-issue.sh RESULT
#
# RESULT is the proof matrix's needs.<job>.result. The environment names the
# run: GH_REPO (owner/repo), RUN_ID, RUN_ATTEMPT, and GITHUB_SERVER_URL
# (default https://github.com). gh reads GH_TOKEN and GH_REPO itself.
#
# Exit 2 is a usage error; any other failure is a failed gh call.

set -euo pipefail

if [ $# -ne 1 ] || [ -z "$1" ]; then
	echo "usage: $0 RESULT" >&2
	exit 2
fi
result="$1"
for var in GH_REPO RUN_ID RUN_ATTEMPT; do
	if [ -z "${!var:-}" ]; then
		echo "$0: $var must be set" >&2
		exit 2
	fi
done

label="slot-release-proof"
title="Scheduled slot release proof failed"
run_url="${GITHUB_SERVER_URL:-https://github.com}/$GH_REPO/actions/runs/$RUN_ID"
today=$(date -u +%Y-%m-%d)

gh label create "$label" --force --color B60205 \
	--description "The scheduled Slot Release Proof workflow did not pass"

# The oldest open issue with the label, if any.
open=$(gh issue list --label "$label" --state open --json number --jq 'map(.number) | min // empty')

if [ "$result" = "success" ]; then
	if [ -z "$open" ]; then
		echo "the proof passed and no issue is open"
		exit 0
	fi
	gh issue close "$open" --comment "The scheduled run on $today passed on every major: $run_url"
	exit 0
fi

# The majors whose proof job did not pass in this attempt, from the jobs' names
# ("slot release proof (PostgreSQL 15)"). The report job itself is still
# running and has no such name, so it is never counted.
if jobs=$(gh api --paginate "repos/$GH_REPO/actions/runs/$RUN_ID/attempts/$RUN_ATTEMPT/jobs" \
	--jq '.jobs[] | select(.conclusion != "success") | .name'); then
	majors=$(printf '%s\n' "$jobs" | sed -n 's/^slot release proof (PostgreSQL \([0-9][0-9]*\))$/\1/p' | paste -sd ' ' -)
	majors="${majors:-none of the per-major jobs failed (result: $result)}"
else
	majors="unknown: listing the run's jobs failed; open the run to see them"
fi

body="The scheduled Slot Release Proof on $today did not pass (result: $result).

- Run: $run_url
- Majors that did not pass: $majors

A red major is the first place to look when its slot release cadence may have
changed, but on a shared runner it can also be contention or an image pull:
read the slot positions the job logs before changing the lag-ceiling sizing
rule (docs/testing.md, the make slot-release-proof row). The next scheduled run
that passes on every major closes this issue."

if [ -n "$open" ]; then
	gh issue comment "$open" --body "$body"
else
	gh issue create --title "$title" --label "$label" --body "$body"
fi
