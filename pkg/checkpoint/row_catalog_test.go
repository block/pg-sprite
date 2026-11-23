package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/internal/testutil"
)

// The store's row reads and writes run on whatever pool the caller passes,
// so they cannot rely on the session's search_path: the position's casts
// and every comparison are spelled through pg_catalog (CO-9). The save is
// checked by its guard, which holds its comparisons, and its position cast;
// its SET list assigns and its COALESCE is grammar, neither resolved
// through the path. This pins the SQL text itself; the integration test
// proves the behaviour under a lookalike text type.
func TestRowStatementsQualifyEveryCastAndOperator(t *testing.T) {
	assert.Contains(t, saveSQL, saveGuardSQL)
	assert.Contains(t, saveSQL, "$8::pg_catalog.pg_lsn")
	assert.NotContains(t, saveSQL, "$8::pg_lsn")
	for name, query := range map[string]string{
		"loadSQL":      loadSQL,
		"saveGuardSQL": saveGuardSQL,
		"deleteSQL":    deleteSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query, tableIdent)
		})
	}
}
