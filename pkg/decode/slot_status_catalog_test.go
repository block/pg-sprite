package decode

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The slot inspection runs on whatever pool the caller passes, and its Own
// column is the ownership check the drop and resume decide on, so it
// cannot rely on the session's search_path: it spells every relation,
// function, cast, and operator through pg_catalog (CO-9). This pins the
// SQL text itself; the integration tests prove the behaviour under
// lookalike operators.
func TestSlotStatusReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	testutil.AssertCatalogQualified(t, inspectSlotSQL)
}
