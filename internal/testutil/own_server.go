package testutil

import (
	"os"
	"testing"
)

// SkipOnSharedServer skips a test that needs a PostgreSQL server of its
// own — one that creates a cluster-level role or a tablespace, or checks
// which image the harness chose — when PG_DSN points the suite at a shared
// long-lived server. reason names what the test does to the cluster.
//
// The long-lived-database targets (make test-db and the CI matrix) never
// run such a test, so make test-own-server runs exactly these tests the
// container way; the own-server completeness test holds that target's
// list to the set of callers of this helper.
func SkipOnSharedServer(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("PG_DSN") != "" {
		t.Skipf("%s; needs a server of its own, not the shared PG_DSN server", reason)
	}
}
