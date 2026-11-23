package schemachange

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The swap reads the WAL insert position inside its own transaction, so a
// pinned search_path protects it today; spelling the function and the cast
// through pg_catalog keeps the position whole if that pin ever moves (CO-9).
func TestSwapPositionReadQualifiesItsFunctionAndCast(t *testing.T) {
	testutil.AssertCatalogQualified(t, swapPositionSQL)
}
