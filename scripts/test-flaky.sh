#!/bin/bash
#
# Proves a flake fix holds by running one test repeatedly.
#
# Usage: scripts/test-flaky.sh <TestName> [iterations] [package]
#
#   scripts/test-flaky.sh TestBuildIndexConcurrently
#   scripts/test-flaky.sh TestBuildIndexConcurrently 20 ./pkg/executor/...
#
# Fails fast on the first failing iteration. Environment variables
# (PG_VERSION, PG_DSN, SKIP_INTEGRATION) pass through to `go test`.

set -euo pipefail

TEST_NAME="${1:?usage: scripts/test-flaky.sh <TestName> [iterations] [package]}"
ITERATIONS="${2:-10}"
PACKAGE="${3:-./...}"

for i in $(seq 1 "$ITERATIONS"); do
    echo "=== iteration $i/$ITERATIONS: $TEST_NAME ==="
    if ! go test -race -count=1 -run "^${TEST_NAME}$" "$PACKAGE"; then
        echo "FAILED on iteration $i/$ITERATIONS"
        exit 1
    fi
done

echo "PASSED all $ITERATIONS iterations"
