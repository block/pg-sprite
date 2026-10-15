package schemachange_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// runnerFixture is a shadow fixture with the checkpoint store a Runner
// writes through.
type runnerFixture struct {
	shadowFixture
	store *checkpoint.Store
}

func newRunnerFixture(t *testing.T) runnerFixture {
	t.Helper()
	return newRunnerFixtureOn(t, testutil.StartPostgres(t))
}

// newRunnerFixtureOn is newRunnerFixture over a server the test started
// itself, for a test that needs the server configured.
func newRunnerFixtureOn(t *testing.T, url string) runnerFixture {
	t.Helper()
	cfg := dbconn.Config{URL: url}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := shadowFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	return runnerFixture{shadowFixture: f, store: store}
}

// runOptions sizes every phase to the fixed thousand-row chunks of the
// gate tests, with opts' other fields laid over.
func (f runnerFixture) runOptions(opts schemachange.RunOptions) schemachange.RunOptions {
	opts.Copier.Workers = 2
	opts.Copier.Chunker = gateChunks
	opts.Checksum.Chunker = gateChunks
	return opts
}

// runner builds a Runner over the fixture's database and store.
func (f runnerFixture) runner(t *testing.T, opts schemachange.RunOptions) *schemachange.Runner {
	t.Helper()
	r, err := schemachange.NewRunner(f.cfg, f.store, f.runOptions(opts))
	require.NoError(t, err)
	return r
}

// run carries table through alter with a fresh Runner over opts.
func (f runnerFixture) run(t *testing.T, table, alter string, opts schemachange.RunOptions) (schemachange.Result, error) {
	t.Helper()
	return f.runner(t, opts).Run(t.Context(), f.pool, f.prove(t, table), f.alter(t, alter))
}

// hasColumn reports whether table has a column named column.
func (f runnerFixture) hasColumn(t *testing.T, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped)`,
		f.schema, table, column).Scan(&exists))
	return exists
}

// inspectRow reads the target's checkpoint row as the run left it.
func (f runnerFixture) inspectRow(t *testing.T, table string) checkpoint.Checkpoint {
	t.Helper()
	cp, err := f.store.Inspect(t.Context(), f.schema, table)
	require.NoError(t, err)
	return cp
}

// One Run carries a quiesced table from no row to done: the shadow is
// built from the statement, filled, verified through the whole key space,
// gated, swapped in under the source's name, and the retained source
// dropped. The live table has the statement's shape and every row the
// source had; the row records the swap's fingerprints in PhaseDone.
func TestRunnerCarriesAQuiescedTableFromNoRowToDone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	liveOID := f.relationOID(t, "orders")

	result, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.True(t, result.Checkpoint.Watermark.Complete(), "the row carries the complete watermark")
	assert.Equal(t, result.Shadow.SourceFingerprint, result.Checkpoint.SourceFingerprint)
	assert.Equal(t, result.Shadow.TargetFingerprint, result.Checkpoint.TargetFingerprint)
	assert.Equal(t, result.Shadow.ShadowOID, result.Swapped.LiveOID, "the shadow the run built is the live table")
	assert.Equal(t, liveOID, result.Swapped.OldOID, "the source the run started from was retained as old")
	assert.Equal(t, 3, result.Verification.Chunks, "2500 rows in thousand-row chunks is three chunks")
	assert.True(t, result.Verification.Clean())
	assert.True(t, result.OldTableDropped)

	assert.Equal(t, result.Shadow.ShadowOID, f.relationOID(t, "orders"), "the live table under the source's name is the shadow")
	assert.False(t, f.hasColumn(t, "orders", "note"), "the gated statement's shape is live")
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is dropped")
	assert.False(t, f.relationExists(t, result.Shadow.ShadowTable), "nothing bears the shadow's name")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t, "orders").Phase)
}

// A retype runs the same path: the live table's key is bigint afterwards,
// with the serial sequence re-owned to it.
func TestRunnerRetypesTheKeyOfAQuiescedTable(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	f.exec(t, `ALTER TABLE %s.orders ALTER COLUMN id TYPE integer`)
	require.Equal(t, "integer", f.columnType(t, "orders", "id"))

	result, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN id TYPE bigint`, schemachange.RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "id"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.Equal(t, f.schema+".orders_id_seq", f.serialSequence(t, "orders", "id"), "the key's sequence is the source's, re-owned")
	assert.True(t, result.OldTableDropped)
}

// A retype that names its conversion runs the same path: the copy converts
// each row with the statement's USING expression, and the live table holds
// the converted values. integer to boolean has no assignment cast, so the
// statement needs USING and so does the copy: a copy that inserted the
// source's integers as they are would be refused by the server. The
// expression is an explicit cast of the column itself, which the verifier's
// own cast of the source agrees with, so the whole key space verifies clean.
func TestRunnerRetypesAColumnThroughTheStatementsUsing(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	f.exec(t, `ALTER TABLE %s.orders ADD COLUMN paid integer`)
	f.exec(t, `UPDATE %s.orders SET paid = qty % 2`)
	f.exec(t, `ALTER TABLE %s.orders ALTER COLUMN paid SET NOT NULL`)

	result, err := f.run(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN paid TYPE boolean USING paid::boolean`, schemachange.RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, map[string]string{"paid": "paid::boolean"}, result.Shadow.Conversions)
	assert.Equal(t, "boolean", f.columnType(t, "orders", "paid"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))
	assert.True(t, result.Verification.Clean())
	var paid int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` WHERE paid`).Scan(&paid))
	assert.EqualValues(t, 1250, paid, "the odd quantities converted to true, the even ones to false")
}

// KeepOld leaves the retained source under its derived old name for the
// caller's rollback window, and the run still reaches done. With both
// relations present, the live table is checked row for row against the
// source it replaced.
func TestRunnerKeepsTheOldTableWhenAsked(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)

	result, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{KeepOld: true})
	require.NoError(t, err)

	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.False(t, result.OldTableDropped)
	old := schemachange.OldName(f.schema, "orders")
	require.True(t, f.relationExists(t, old), "the retained source stays under its old name")
	assert.True(t, f.hasColumn(t, old, "note"), "the old table keeps the source's shape")
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: old},
		testutil.RelationRef{Schema: f.schema, Table: "orders"},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
}

// A run whose row is already done is refused as terminal and changes
// nothing: the run it records has ended, and a caller that means to run
// the table again deletes the row first.
func TestRunnerRefusesARowAlreadyDone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{})
	require.NoError(t, err)
	before := f.inspectRow(t, "orders")

	_, err = f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeTerminal, schemachange.RefusalCauseOf(err))
	assert.Equal(t, before, f.inspectRow(t, "orders"), "a refused run leaves the row as it found it")
	assert.True(t, f.hasColumn(t, "orders", "sku"), "nothing was built for the second statement")
}

// AfterCheckpoint sees every save in order, with the row as saved, so a
// caller can mirror the run's progress; the watermark it sees at
// verifying is complete and the phases arrive in order.
func TestRunnerReportsEverySaveInOrder(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	var phases []checkpoint.Phase
	var atVerifying copier.Watermark
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		phases = append(phases, cp.Phase)
		if cp.Phase == checkpoint.PhaseVerifying {
			atVerifying = cp.Watermark
		}
		return nil
	}}

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, opts)
	require.NoError(t, err)

	// The first save is the fresh row and the last the done row; the copy
	// may add further copying saves between, depending on how far the
	// copier got before each interval.
	require.NotEmpty(t, phases)
	assert.Equal(t, checkpoint.PhaseCopying, phases[0])
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone}, phases[len(phases)-3:])
	assert.True(t, atVerifying.Complete())
}
