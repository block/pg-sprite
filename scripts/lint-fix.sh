#!/bin/bash
#
# Runs golangci-lint with --fix on staged Go files.
# Used by the pre-commit hook.
#
# Auto-fixable issues (formatting, imports, misspellings) are fixed
# automatically. Non-auto-fixable issues require manual fixes before
# committing.
#
# How it works:
#
#   1. User stages file:         git add engine.go
#   2. User commits:             git commit  (triggers pre-commit hook)
#   3. Hook runs lint --fix:     golangci-lint run --fix  (fixes working tree)
#   4. Hook re-stages:           git add engine.go  (staging area gets fixed version)
#   5. Hook verifies:            golangci-lint run  (confirms no remaining issues)
#   6. Commit proceeds with the fixed version

set -e

STAGED_GO_FILES=$(git diff --cached --name-only --diff-filter=ACM | grep '\.go$' || true)

if [ -z "$STAGED_GO_FILES" ]; then
    exit 0
fi

# Re-stage any files the fixers changed.
restage_fixed_files() {
    for file in $STAGED_GO_FILES; do
        if [ -f "$file" ] && ! git diff --quiet "$file" 2>/dev/null; then
            echo "Auto-fixed: $file"
            git add "$file"
        fi
    done
}

# Use local golangci-lint if available, otherwise Docker.
# Check common Go binary paths since git hooks may not inherit the full user PATH.
LINT_CMD=""
for candidate in golangci-lint "$HOME/go/bin/golangci-lint" "$GOPATH/bin/golangci-lint" "$GOBIN/golangci-lint"; do
    if command -v "$candidate" >/dev/null 2>&1; then
        LINT_CMD="$candidate"
        break
    fi
done
if [ -z "$LINT_CMD" ]; then
    LINT_CMD="docker run --rm -v $(pwd):/app -w /app golangci/golangci-lint:latest golangci-lint"
fi

# Detect the merge-base so we only flag issues introduced by this branch.
# If merge-base equals HEAD (e.g., after git reset --soft for squashing),
# skip --new-from-rev to avoid treating every changed line as "new".
NEW_FROM_REV=""
for base_branch in origin/main origin/master; do
    if git rev-parse --verify "$base_branch" >/dev/null 2>&1; then
        MERGE_BASE=$(git merge-base HEAD "$base_branch" 2>/dev/null || true)
        if [ -n "$MERGE_BASE" ] && [ "$MERGE_BASE" != "$(git rev-parse HEAD)" ]; then
            NEW_FROM_REV="$MERGE_BASE"
        fi
        break
    fi
done

new_flag=""
if [ -n "$NEW_FROM_REV" ]; then
    new_flag="--new-from-rev=$NEW_FROM_REV"
fi

# Lint the packages containing staged files: auto-fix, re-stage, then verify.
PACKAGES=$(echo "$STAGED_GO_FILES" | xargs -n1 dirname | sort -u | sed 's|^|./|' | sed 's|$|/...|')

echo "Running golangci-lint --fix..."
# shellcheck disable=SC2086
$LINT_CMD run --fix --timeout=5m $PACKAGES || true

restage_fixed_files

# shellcheck disable=SC2086
if ! $LINT_CMD run --timeout=5m $new_flag $PACKAGES; then
    echo ""
    echo "golangci-lint found issues that cannot be auto-fixed."
    echo "Please fix them manually before committing."
    exit 1
fi

echo "All lint checks passed!"
