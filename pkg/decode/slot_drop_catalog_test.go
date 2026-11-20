package decode

import (
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
)

// The drop runs on whatever pool the caller passes, so the reads it decides
// on cannot rely on the session's search_path: the ownership check, the
// server proof before every slot command, and the publication reads that
// judge whether the publication of the name is the route's all spell every
// relation, function, cast, and operator through pg_catalog (CO-9), and so
// do the slot inspection and the WAL marker, which run on the caller's
// pool too, and the heartbeat's read of wal_sender_timeout, which runs on
// the replication session under whatever path the caller's settings give
// it. This pins the SQL text itself; the integration tests prove the
// behaviour under lookalike operators and types.
func TestSlotDropReadsQualifyEveryRelationFunctionCastAndOperator(t *testing.T) {
	for name, query := range map[string]string{
		"slotIsOwnSQL":                   slotIsOwnSQL,
		"catalogIdentitySQL":             catalogIdentitySQL,
		"onTargetDatabaseSQL":            onTargetDatabaseSQL,
		"publicationShapeSQL":            publicationShapeSQL,
		"publishedTablesSQL(no filters)": publishedTablesSQL(false),
		"publishedTablesSQL(filters)":    publishedTablesSQL(true),
		"publishedSchemasSQL":            publishedSchemasSQL,
		"inspectSlotSQL":                 inspectSlotSQL,
		"markWALSQL":                     markWALSQL,
		"senderTimeoutSQL":               senderTimeoutSQL,
	} {
		t.Run(name, func(t *testing.T) {
			testutil.AssertCatalogQualified(t, query)
		})
	}
}
