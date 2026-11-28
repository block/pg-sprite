package checkpoint

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The reaper and the slot lock run on whatever pool the caller passes, so
// their reads cannot rely on the session's search_path: every relation
// other than the engine's own table, and every function, cast, and
// operator they spell, is pg_catalog-qualified (CO-9). This pins the SQL
// text itself; the integration tests prove the behaviour under lookalike
// operators.
func TestSlotReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	for name, query := range map[string]string{
		"candidateSlotsSQL":   candidateSlotsSQL,
		"liveRowSlotsSQL":     liveRowSlotsSQL,
		"unfinishedSQL":       unfinishedSQL,
		"databaseIdentitySQL": databaseIdentitySQL,
		"slotLockSQL":         slotLockSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query, tableIdent)
		})
	}
}
