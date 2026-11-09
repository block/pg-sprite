package migrate

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A row security file whose table is dropped after its row security was
// compared reaches the delegated run with a table that does not exist. The
// run refuses it rather than creating the table from its definition alone,
// which would leave a table without its row security, and nothing is
// created.
func TestRunDesiredRefusesATableDroppedAfterItsRowSecurityWasCompared(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE t (id int PRIMARY KEY);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)

	opts := DefaultOptions()
	res, err := runDesired(t.Context(), pool, DesiredRequest{Schema: schema, Desired: desired.TableDefinition()}, opts, refuseDroppedTable)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, res.Class)
	assert.Equal(t, schema+".t was dropped after its row security was compared; create it with its row security through executor.ExecuteDesiredRowSecurity; nothing was executed", res.Detail)
	assert.Empty(t, res.Verdicts)

	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{schema, "t"}.Sanitize()).Scan(&exists))
	assert.False(t, exists, "the table is not created")
}
