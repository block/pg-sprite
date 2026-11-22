package decode

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The drop runs on whatever pool the caller passes, so the reads it decides
// on cannot rely on the session's search_path: the server proof before
// every slot command and the publication reads that judge whether the
// publication of the name is the route's all spell every relation,
// function, cast, and operator through pg_catalog (CO-9). The ownership
// check is the slot inspection's, pinned beside it. This pins the SQL text
// itself; the integration tests prove the behaviour under lookalike
// operators.
func TestSlotDropReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	for name, query := range map[string]string{
		"catalogIdentitySQL":             catalogIdentitySQL,
		"onTargetDatabaseSQL":            onTargetDatabaseSQL,
		"publicationShapeSQL":            publicationShapeSQL,
		"publishedTablesSQL(no filters)": publishedTablesSQL(false),
		"publishedTablesSQL(filters)":    publishedTablesSQL(true),
		"publishedSchemasSQL":            publishedSchemasSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query)
		})
	}
}
