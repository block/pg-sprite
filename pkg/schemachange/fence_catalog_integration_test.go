package schemachange_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The fence function runs in the writer's session, under the writer's own
// search_path, not one the engine set (CO-9). A writer whose search_path lists
// a schema ahead of pg_catalog must still be refused with the fence's
// SQLSTATE, whatever that schema holds: here an impostor format() that
// answers NULL, which as the message of the refusal would raise a null-value
// error of its own in place of the fence's.
func TestWriteFenceRefusalIgnoresAWritersShadowingSearchPath(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	stop := &stopAfter{phase: checkpoint.PhaseCopying}
	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.True(t, f.hasFence(t, "orders"), "the stopped run left the fence in place")

	impostors := testutil.NewSchema(t, f.pool)
	testutil.ExecInSchema(t, f.pool, impostors, `
		CREATE FUNCTION format(text, name, name) RETURNS text
		LANGUAGE sql AS 'SELECT NULL::text'`)
	testutil.ExecInSchema(t, f.pool, impostors, `
		CREATE FUNCTION format(text, text, text) RETURNS text
		LANGUAGE sql AS 'SELECT NULL::text'`)
	writer := testutil.NewCatalogShadowingPool(t, f.cfg.URL, impostors)

	_, err = writer.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` (qty) VALUES (1)`)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the fenced table refuses the write")
	assert.Equal(t, schemachange.WriteFenceSQLState, pgErr.Code, "the refusal carries the fence's SQLSTATE, not the impostor's")
}
