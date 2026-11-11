#!/bin/bash
#
# Fail when the code calls into a known vulnerability.
#
# This gate is reachability-based on purpose. govulncheck sorts findings into
# three tiers: symbols the build actually calls, packages it imports without
# calling the vulnerable symbol, and modules present in the graph at all. Only
# the first tier fails here, which is also govulncheck's own exit contract.
#
# Gating on the lower tiers is not a stricter version of this check, it is an
# unsatisfiable one: an advisory can be published against a module with no
# fixed version to move to, and the build would then be red with no action
# available to turn it green. Those findings still print, so a scan that goes
# quiet about them is still visible in the log.
#
# A failure here is usually not caused by the diff that triggered it. The
# advisory database is external and moves on its own, so a graph that scanned
# clean yesterday goes red today with no commit in between. The remedy is a
# version bump, not a code change: the go directive in go.mod for a finding in
# the standard library, the module's own version for a finding in a dependency.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Pinned so the scanner is reproducible. The vulnerability database it reads
# is live by design, and is the part that is meant to move.
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.6.0}"

GOBIN="$(go env GOPATH)/bin"
GOVULNCHECK="${GOBIN}/govulncheck"

# Reuse an existing binary only when it is the pinned version. A developer who
# already has a different govulncheck installed would otherwise get a different
# answer from the one CI gets, from the same command, with nothing on screen
# saying why.
installed=""
if [ -x "$GOVULNCHECK" ]; then
    installed="$("$GOVULNCHECK" -version 2>/dev/null | awk '/^Scanner:/ { print $2 }')" || installed=""
fi

if [ "$installed" != "govulncheck@${GOVULNCHECK_VERSION}" ]; then
    echo "Installing govulncheck ${GOVULNCHECK_VERSION}..."
    GOFLAGS=-mod=mod go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
fi

if ! "$GOVULNCHECK" ./...; then
    echo ""
    echo "Reachable vulnerabilities found. Bump the affected modules to the"
    echo "release named in the advisory, and check that release for advisories"
    echo "of its own before landing on it."
    exit 1
fi

echo "No reachable vulnerabilities."
