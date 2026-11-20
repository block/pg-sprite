package applier

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The catch-up reads the server's write position and snapshot on the
// caller's pool every cycle, so the reads cannot rely on the session's
// search_path: every function and cast they spell is pg_catalog-qualified
// (CO-9). This pins the SQL text itself; the integration test proves the
// behaviour under a lookalike text type.
func TestCatchupPoolReadsQualifyEveryFunctionAndCast(t *testing.T) {
	for name, query := range map[string]string{
		"walEndSQL":   walEndSQL,
		"snapshotSQL": snapshotSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query)
		})
	}
}
