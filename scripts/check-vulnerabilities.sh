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
# is live by design, and is the part that is meant to move. Renovate bumps
# this pin (see renovate.json).
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.6.0}"

# govulncheck judges the standard library of the toolchain go resolves to.
# Pinning that toolchain to the go directive makes a local run judge the same
# standard library CI does, which installs exactly that version. The directive
# is read under GOTOOLCHAIN=auto so an older local toolchain can read it too.
GOTOOLCHAIN="go$(GOTOOLCHAIN=auto go list -m -f '{{.GoVersion}}')"
export GOTOOLCHAIN

# The scanner lives in a directory named for its version, which this script
# both installs to and runs from, so a govulncheck installed anywhere else
# never answers in its place. GOFLAGS is set for the install only: it lets the
# install work under a vendoring GOFLAGS, and must not reach the scan, where
# -mod=mod would let go list rewrite go.mod.
GOVULNCHECK_DIR="bin/govulncheck-${GOVULNCHECK_VERSION}"
GOVULNCHECK="${GOVULNCHECK_DIR}/govulncheck"
if [ ! -x "$GOVULNCHECK" ]; then
    echo "Installing govulncheck ${GOVULNCHECK_VERSION}..."
    GOBIN="$PWD/$GOVULNCHECK_DIR" GOFLAGS=-mod=mod \
        go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
fi

# govulncheck exits 3 when the code calls a vulnerable symbol and 1 when the
# scan itself could not run (advisory fetch, package load, toolchain); only
# the first is fixed by a version bump.
status=0
"$GOVULNCHECK" ./... || status=$?
case "$status" in
0)
    echo "No reachable vulnerabilities."
    ;;
3)
    echo ""
    echo "Reachable vulnerabilities found. Bump the affected modules to the"
    echo "release named in the advisory, and check that release for advisories"
    echo "of its own before landing on it."
    exit 3
    ;;
*)
    echo ""
    echo "govulncheck could not complete the scan (exit ${status}); the output"
    echo "above names the cause. This is not a vulnerability finding."
    exit "$status"
    ;;
esac
