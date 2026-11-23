package executor

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The partitioned index build reads the catalog on the caller's pool, whatever
// its search_path, so every relation other than the CTE a query defines, and
// every function, cast, and operator it spells, is pg_catalog-qualified (CO-9).
// This pins the SQL text itself; the integration tests prove the behavior.
func TestPartitionedIndexCatalogQueriesQualifyEverythingTheyName(t *testing.T) {
	for name, query := range map[string]string{
		"listPartitionTreeSQL":   listPartitionTreeSQL,
		"partitionKeyColumnsSQL": partitionKeyColumnsSQL,
		"relationByNameSQL":      relationByNameSQL,
		"relationByOIDSQL":       relationByOIDSQL,
		"attachedIndexOfSQL":     attachedIndexOfSQL,
		"relkindSQL":             relkindSQL,
		"txBudgetsSQL":           txBudgetsSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query, "tree")
		})
	}
}
