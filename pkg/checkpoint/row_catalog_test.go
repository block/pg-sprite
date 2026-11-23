package checkpoint

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The store's row reads and writes run on whatever pool the caller passes,
// so they cannot rely on the session's search_path: the position's casts
// and every comparison are spelled through pg_catalog (CO-9). The save's
// SET list assigns and its COALESCE is grammar, so neither is judged. This
// pins the SQL itself; the integration test proves the behaviour under a
// lookalike text type.
func TestRowStatementsQualifyEveryCastAndOperator(t *testing.T) {
	for name, query := range map[string]string{
		"loadSQL":   loadSQL,
		"saveSQL":   saveSQL,
		"deleteSQL": deleteSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query, tableIdent)
		})
	}
}
