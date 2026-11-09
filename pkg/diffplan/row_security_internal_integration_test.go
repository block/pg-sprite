package diffplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The table definition of a row security file is planned only after its row
// security matched the live table. A table dropped between the two is
// reported as missing, never planned as a create of the table without its
// row security.
func TestPlanTableDefinitionReportsATableDroppedAfterItsRowSecurityMatched(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE t (id int PRIMARY KEY);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)

	report, err := planTableDefinition(t.Context(), pool, schema, desired)
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	assert.ErrorContains(t, err, schema+".t was dropped while its table definition was planned")
	assert.Empty(t, report.Statements)
}
