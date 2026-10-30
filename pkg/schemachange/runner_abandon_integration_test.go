package schemachange_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// The tests here drive the Runner into a data fault — the server rejecting
// the source's rows as the statement would shape them — and check that the
// run abandons itself: the shadow, the hold on the source, and the
// checkpoint row are discarded before the error returns, so the next run
// of a corrected statement starts from the beginning with nothing to
// acknowledge, and the source is untouched and writable throughout.

// stringTooLong is the SQLSTATE the server raises when a value does not
// fit the column's declared length (string_data_right_truncation).
const stringTooLong = "22001"

// A quiesced run whose copy meets a value the new type will not hold —
// every sku is four characters, varchar(2) holds two — stops on a data
// fault and is abandoned: the error is an AbandonedError that still
// unwraps to the server's SQLSTATE, the shadow is gone, the fence is off
// the source, no row remains, and the source is as it was. A plain run of
// a corrected statement then carries the table to done with no restart
// acknowledgement, since nothing of the abandoned run is left to discard.
func TestRunnerAbandonsAQuiescedRunOnADataFault(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	sourceOID := f.relationOID(t, "orders")

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN sku TYPE varchar(2)`, schemachange.RunOptions{})
	var abandoned *schemachange.AbandonedError
	require.ErrorAs(t, err, &abandoned)
	assert.Equal(t, f.schema, abandoned.Schema)
	assert.Equal(t, "orders", abandoned.Table)
	assert.True(t, schemachange.IsDataFault(err), "the abandoned error still reads as the fault")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, stringTooLong, pgErr.Code)

	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the shadow is dropped")
	assert.False(t, f.hasFence(t, "orders"), "the fence is off the source")
	f.assertWritable(t, "orders")
	_, err = f.store.Inspect(t.Context(), f.schema, "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "no row remains")
	assert.Equal(t, sourceOID, f.relationOID(t, "orders"), "the source is the table it was")
	assert.Equal(t, "text", f.columnType(t, "orders", "sku"), "the source keeps its shape")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))

	result, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN sku TYPE varchar(4)`, schemachange.RunOptions{})
	require.NoError(t, err, "a corrected statement runs plain, with nothing to acknowledge")
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, sourceOID, result.Swapped.OldOID, "the source the abandoned run fenced is the one swapped out")
	assert.Equal(t, "character varying(4)", f.columnType(t, "orders", "sku"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}

// A data fault whose abandon fails leaves the run held, not abandoned: a
// view another session builds on the shadow while the run copies makes the
// discard's drop of the shadow (never CASCADE) fail, so the error is the
// fault joined with the discard's failure — still a data fault by its
// SQLSTATE, but no AbandonedError — and the row, the fence, and the shadow
// all stand. Once the view is gone, the restart acknowledgement finishes
// the discard and runs a corrected statement to done.
func TestRunnerHoldsADataFaultWhoseAbandonFails(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	pinShadow := func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying {
			return nil
		}
		_, err := f.pool.Exec(ctx, fmt.Sprintf(`
			CREATE VIEW %s AS
			SELECT id
			FROM %s`,
			pgx.Identifier{f.schema, "orders_shadow_reader"}.Sanitize(),
			pgx.Identifier{f.schema, shadow}.Sanitize()))
		return err
	}

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN sku TYPE varchar(2)`, schemachange.RunOptions{AfterCheckpoint: pinShadow})
	var abandoned *schemachange.AbandonedError
	assert.False(t, errors.As(err, &abandoned), "a discard that failed is not an abandonment")
	assert.True(t, schemachange.IsDataFault(err), "the error still reads as the fault")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, stringTooLong, pgErr.Code, "the fault's SQLSTATE is the first the error yields")
	assert.ErrorContains(t, err, "abandon run on "+f.schema+".orders")

	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the row stands")
	assert.True(t, f.hasFence(t, "orders"), "the fence stands")
	assert.True(t, f.relationExists(t, shadow), "the shadow stands")

	f.exec(t, `DROP VIEW %s.orders_shadow_reader`)
	result, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN sku TYPE varchar(4)`, schemachange.RunOptions{Restart: true})
	require.NoError(t, err, "the restart finishes the discard and runs the corrected statement")
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, "character varying(4)", f.columnType(t, "orders", "sku"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
}

// A live run's hold on the source is its replication slot, and the slot
// pins WAL on the server for as long as it stands. A data fault in a live
// run's copy — every label is at least five characters, varchar(4) holds
// four — is abandoned the same way, with the slot dropped in place of the
// fence.
func TestRunnerAbandonsALiveRunOnADataFault(t *testing.T) {
	f := newDecodingFixture(t, 100)
	st, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN label TYPE varchar(4)`, f.table.Qualified()))
	require.NoError(t, err)
	r, err := schemachange.NewRunner(f.cfg, f.store, liveRunOptions(schemachange.RunOptions{}))
	require.NoError(t, err)

	_, err = r.Run(t.Context(), f.pool, f.target, st)
	var abandoned *schemachange.AbandonedError
	require.ErrorAs(t, err, &abandoned)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, stringTooLong, pgErr.Code)

	_, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found, "the slot is dropped")
	_, err = f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "no row remains")
	var shadowExists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2)`,
		f.table.Schema, schemachange.ShadowName(f.table.Schema, f.table.Table)).Scan(&shadowExists))
	assert.False(t, shadowExists, "the shadow is dropped")
	assert.Equal(t, "text", f.labelType(t), "the source keeps its shape")
	_, err = f.pool.Exec(t.Context(), `INSERT INTO `+f.table.Qualified()+` (uniq, amount, label) VALUES (100001, 1.25, 'after-abandon')`)
	assert.NoError(t, err, "the source takes writes")
}

// A stop that is not a data fault is not abandoned, whatever else it is:
// a verification difference leaves the row in verifying with the fence
// standing, for the operator's restart acknowledgement to discard.
func TestRunnerDoesNotAbandonAStopOutsideTheDataClasses(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	stop := &stopAfter{phase: checkpoint.PhaseVerifying}

	_, err := f.run(t, "orders", restartAlter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	stopped := f.inspectRow(t, "orders")
	f.exec(t, `UPDATE %s.`+stopped.ShadowTable+` SET qty = 0 WHERE id = 5`)

	_, err = f.run(t, "orders", restartAlter, schemachange.RunOptions{})
	var abandoned *schemachange.AbandonedError
	assert.False(t, errors.As(err, &abandoned), "a divergence is the operator's to act on, not the Runner's")
	assert.False(t, schemachange.IsDataFault(err))
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t, "orders").Phase, "the row stands where the stop left it")
	assert.True(t, f.hasFence(t, "orders"), "the fence stands")
	assert.True(t, f.relationExists(t, stopped.ShadowTable), "the shadow stands")
}

// labelType is the declared type of the workload table's label column.
func (f reconcileFixture) labelType(t *testing.T) string {
	t.Helper()
	var typ string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = 'label'`,
		f.table.Schema, f.table.Table).Scan(&typ))
	return typ
}
