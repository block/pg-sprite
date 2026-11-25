package migrate

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here stop a copy-and-swap run through the front door and then
// run a statement on the same table again. The front door routes a
// statement by the table's current shape, which a stopped run may already
// have changed, so it looks for the stopped run first: the same statement
// finishes the run through copy-and-swap, and any other statement is
// refused until the stopped run is finished or started over.

// crashAfterSwap runs st through the front door and makes it die right
// after its swap commits: when the run saves cutover, a second session
// takes the checkpoint row FOR UPDATE and holds it, so the swap commits
// and the done save then fails on its lock timeout, leaving the row in
// cutover. KeepOld keeps the retained source on the volume, so the run the
// test makes next has the old table to drop.
func (f copySwapFixture) crashAfterSwap(t *testing.T, st statement.Statement, opts Options) {
	t.Helper()
	opts.CopySwap.KeepOld = true
	var holder pgx.Tx
	opts.CopySwap.OnCheckpoint = func(ctx context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCutover {
			return nil
		}
		tx, err := f.pool.Begin(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
		holder = tx
		_, err = tx.Exec(ctx, `
			SELECT 1
			FROM pgsprite.pgsprite_checkpoint
			FOR UPDATE`)
		return err
	}
	v, err := Run(t.Context(), f.pool, st, opts)
	require.Error(t, err, "the done save timed out on the held row")
	require.Equal(t, verdict.OutcomeFailed, v.Outcome)
	require.NotNil(t, holder)
	require.NoError(t, holder.Rollback(t.Context()))
	require.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase)
	require.True(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")),
		"the swap committed and the retained source is on the volume")
}

// A copy-and-swap run of
//
//	ALTER TABLE orders DROP COLUMN note, ALTER COLUMN sku TYPE varchar(10)
//
// routes to copy-and-swap because text to varchar(10) rewrites the table.
// A run that dies after its swap committed leaves a table that already has
// the statement's shape: note is gone and sku is varchar(10). Routed
// against that shape the same statement is native — varchar(10) to
// varchar(10) is a relabel — and its drop of a column that no longer
// exists is refused by the server with 42703, so the stopped run is never
// finished. The front door finds the stopped run's cutover row instead and
// sends the statement to copy-and-swap, which reads the committed swap
// from the catalog, drops the retained source, and saves done.
//
// The destructive acknowledgement is asked of the rerun as of any run of a
// statement that drops a column: without it the rerun is refused with
// destructive-change and the stopped run stays as it was.
func TestRunFinishesAStoppedCopySwapWhoseSwapCommitted(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note,
			ALTER COLUMN sku TYPE varchar(10)`))
	require.NoError(t, err)
	opts := f.options()
	opts.AllowDestructive = f.schema + ".orders"
	f.crashAfterSwap(t, st, opts)
	require.False(t, f.hasColumn(t, "orders", "note"), "the swapped-in table has the statement's shape")

	unacknowledged := f.options()
	v, err := Run(t.Context(), f.pool, st, unacknowledged)
	require.NoError(t, err)
	assert.Equal(t, verdict.ReasonDestructiveChange, v.Reason, "%s", v.Detail)
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase, "the refused rerun touched nothing")

	v, err = Run(t.Context(), f.pool, st, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.True(t, v.AllowedDestructive)
	assert.Contains(t, v.Detail, "the retained source is dropped")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "the retained source is dropped")
	assert.Equal(t, "character varying", f.columnType(t, "orders", "sku"))
	assert.EqualValues(t, 2500, f.rowCount(t, "orders"))

	// A done row is the end of the run, so the next statement on the
	// table routes on its own again.
	next, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN sku SET DEFAULT 'none'`))
	require.NoError(t, err)
	v, err = Run(t.Context(), f.pool, next, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s", v.Reason, v.Detail)
}

// A run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// stopped in verifying leaves its shadow, its fence on the source, and its
// row. Another statement on the same table,
//
//	ALTER TABLE orders DROP COLUMN note
//
// routes natively and would drop the column from the source under the
// stopped run, whose shadow would then describe a table that no longer
// exists. The front door refuses it instead, with cause
// copy-and-swap-run-stopped, and executes nothing: note stays, and the
// stopped run's row and shadow stand as they were. Running the stopped
// statement again finishes it, after which the drop runs.
func TestRunRefusesAnotherStatementWhileACopySwapRunIsStopped(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	retype, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	stop := &progressLog{stopAt: checkpoint.PhaseVerifying}
	first := f.options()
	first.CopySwap.OnCheckpoint = stop.hook
	_, err = Run(t.Context(), f.pool, retype, first)
	require.ErrorIs(t, err, errHostStopped)
	require.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)

	drop, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note`))
	require.NoError(t, err)
	dropOpts := f.options()
	dropOpts.AllowDestructive = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, drop, dropOpts)
	require.NoError(t, err, "a refusal is a verdict, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, v.Cause)
	assert.True(t, f.hasColumn(t, "orders", "note"), "nothing was executed")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the stopped run's shadow stands")

	v, err = Run(t.Context(), f.pool, retype, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)

	v, err = Run(t.Context(), f.pool, drop, dropOpts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// The restart acknowledgement is the other way past a stopped run before
// its swap: a statement that itself routes to copy-and-swap,
//
//	ALTER TABLE orders ALTER COLUMN sku TYPE varchar(10)
//
// run with the table named for a restart, is not refused over the stopped
// retype of qty. The copy-and-swap route discards the stopped run — its
// shadow, fence, and row — and runs the new statement from the start, so
// sku changes and qty keeps its type.
func TestRunRestartRunsAnotherStatementOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	retype, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	stop := &progressLog{stopAt: checkpoint.PhaseVerifying}
	first := f.options()
	first.CopySwap.OnCheckpoint = stop.hook
	_, err = Run(t.Context(), f.pool, retype, first)
	require.ErrorIs(t, err, errHostStopped)
	require.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)

	other, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN sku TYPE varchar(10)`))
	require.NoError(t, err)
	opts := f.options()
	opts.CopySwap.Restart = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, other, opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, "character varying", f.columnType(t, "orders", "sku"))
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the stopped retype was discarded, not finished")
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
}

// Desired-state execution meets the same stopped run with nothing to run:
// the table already has the desired shape, so the plan is empty, and
// reporting it converged would leave the cutover row and the retained
// source standing with nothing left to finish them. It is refused with the
// same cause instead, and the imperative rerun of the stopped statement
// finishes it.
func TestRunDesiredRefusesAnEmptyPlanOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	f.crashAfterSwap(t, st, f.options())

	desired, err := statement.ParseDesired(`
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  bigint NOT NULL,
			note text,
			sku  text
		);
		CREATE INDEX orders_qty_idx ON orders (qty);`)
	require.NoError(t, err)
	res, err := RunDesired(t.Context(), f.pool, DesiredRequest{Schema: f.schema, Desired: desired}, DefaultOptions())
	require.NoError(t, err, "a refusal is a result, not an error")
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, "%s: %s", res.Reason, res.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
	assert.Empty(t, res.Plan.Statements, "the table already has the desired shape")
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase)

	v, err := Run(t.Context(), f.pool, st, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, checkpoint.PhaseDone, f.inspectRow(t).Phase)
}

// stopRetypeAtVerifying runs
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// through the front door and stops it, as a host would, when it saves
// verifying: its shadow, its fence on the source, and its row stand.
func (f copySwapFixture) stopRetypeAtVerifying(t *testing.T) statement.Statement {
	t.Helper()
	retype, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	stop := &progressLog{stopAt: checkpoint.PhaseVerifying}
	opts := f.options()
	opts.CopySwap.OnCheckpoint = stop.hook
	_, err = Run(t.Context(), f.pool, retype, opts)
	require.ErrorIs(t, err, errHostStopped)
	require.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase)
	return retype
}

// requireStoppedRunRefusal checks that v is the refusal of a statement over
// a stopped run on orders.
func (f copySwapFixture) requireStoppedRunRefusal(t *testing.T, v verdict.Verdict) {
	t.Helper()
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, v.Cause)
	assert.Equal(t, f.schema+".orders", v.Table)
}

// An index rename names the index, not its table, and is routed before the
// front door reads a table's checkpoint row. Over the retype of qty stopped
// in verifying,
//
//	ALTER INDEX orders_qty_idx RENAME TO orders_qty_renamed
//
// would rename the source's index under the run whose shadow was built to
// match it, and the resume would then refuse the changed source. It is
// refused with copy-and-swap-run-stopped once its owning table is known,
// and the index keeps its name.
func TestRunRefusesAnIndexRenameWhileACopySwapRunIsStopped(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	rename, err := statement.ParseOne(f.inSchema(`
		ALTER INDEX {schema}.orders_qty_idx
			RENAME TO orders_qty_renamed`))
	require.NoError(t, err)
	v, err := Run(t.Context(), f.pool, rename, DefaultOptions())
	require.NoError(t, err)
	f.requireStoppedRunRefusal(t, v)
	assert.True(t, f.relationExists(t, "orders_qty_idx"), "nothing was executed")
	assert.False(t, f.relationExists(t, "orders_qty_renamed"))
}

// A blocking DROP INDEX run under --accept-blocking takes the passthrough,
// which resolves the index's table itself. Over the stopped retype it would
// drop the source's index under the run; it is refused the same way, with
// both acknowledgements given, and the index stays.
func TestRunRefusesAnAcceptedDropIndexWhileACopySwapRunIsStopped(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	drop, err := statement.ParseOne(f.inSchema(`
		DROP INDEX {schema}.orders_qty_idx`))
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.AcceptBlocking = f.schema + ".orders"
	opts.AllowDestructive = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, drop, opts)
	require.NoError(t, err)
	f.requireStoppedRunRefusal(t, v)
	assert.True(t, f.relationExists(t, "orders_qty_idx"), "nothing was executed")
}

// A stopped run's row that this engine cannot read — here one a newer
// engine wrote in another row format, the state a rolling upgrade leaves —
// still holds the table while the run's shadow stands. The native
//
//	ALTER TABLE orders DROP COLUMN note
//
// reads no row of its own, so the front door refuses it rather than drop
// the column under the run, and the shadow and the column stay.
func TestRunRefusesAStatementWhileTheStoppedRunsRowIsOfAnotherFormat(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)
	f.exec(t, `UPDATE pgsprite.pgsprite_checkpoint SET format_version = format_version + 1`)

	drop, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note`))
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.AllowDestructive = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, drop, opts)
	require.NoError(t, err)
	f.requireStoppedRunRefusal(t, v)
	assert.True(t, f.hasColumn(t, "orders", "note"), "nothing was executed")
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the stopped run's shadow stands")
}

// A read of the stopped run's row that fails for any reason but a missing
// row, a missing store, a privilege, or another format is an error, and
// nothing routes. Here the store's slot_name column has another type, so
// the row cannot be scanned:
//
//	ALTER TABLE orders ALTER COLUMN note SET DEFAULT 'x'
//
// returns the read's error, and note keeps no default.
func TestRunStopsWhenTheStoppedRunRowCannotBeRead(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)
	f.exec(t, `
		ALTER TABLE pgsprite.pgsprite_checkpoint
			ALTER COLUMN slot_name TYPE integer USING 0`)

	next, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN note SET DEFAULT 'x'`))
	require.NoError(t, err)
	v, err := Run(t.Context(), f.pool, next, DefaultOptions())
	require.Error(t, err, "outcome=%s reason=%s", v.Outcome, v.Reason)
	assert.Empty(t, v.Outcome, "no verdict: the run stopped before routing")
	var hasDefault bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT column_default IS NOT NULL
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note'`,
		f.schema).Scan(&hasDefault))
	assert.False(t, hasDefault, "nothing was executed")
}

// Desired-state execution with something to run over a stopped run sends
// each statement through the imperative door, which refuses it: after the
// retype of qty committed its swap, a desired schema that also adds extra
// plans
//
//	ALTER TABLE orders ADD COLUMN extra text
//
// and that statement is refused with copy-and-swap-run-stopped, so extra
// is not added.
func TestRunDesiredRefusesANonEmptyPlanOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	f.crashAfterSwap(t, st, f.options())

	desired, err := statement.ParseDesired(`
		CREATE TABLE orders (
			id    bigserial PRIMARY KEY,
			qty   bigint NOT NULL,
			note  text,
			sku   text,
			extra text
		);
		CREATE INDEX orders_qty_idx ON orders (qty);`)
	require.NoError(t, err)
	res, err := RunDesired(t.Context(), f.pool, DesiredRequest{Schema: f.schema, Desired: desired}, DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, "%s: %s", res.Reason, res.Detail)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
	assert.False(t, f.hasColumn(t, "orders", "extra"), "nothing ran over the stopped run")
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase)
}

// The stopped statement rerun with the restart acknowledgement is still
// the stopped run's own statement, so it goes back to copy-and-swap even
// though, against the swapped-in shape, it now routes natively:
//
//	ALTER TABLE orders DROP COLUMN note, ALTER COLUMN sku TYPE varchar(10)
//
// is a relabel once sku is varchar(10). There the restart meets a committed
// swap, which it cannot discard: the run is refused with
// restart-swap-committed and the cutover row stays, rather than the
// acknowledgement being rejected as one for a native statement.
func TestRunRestartOfTheStoppedStatementAfterItsSwapIsRefused(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	st, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note,
			ALTER COLUMN sku TYPE varchar(10)`))
	require.NoError(t, err)
	first := f.options()
	first.AllowDestructive = f.schema + ".orders"
	f.crashAfterSwap(t, st, first)

	opts := f.options()
	opts.AllowDestructive = f.schema + ".orders"
	opts.CopySwap.Restart = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, st, opts)
	require.NoError(t, err, "outcome=%s reason=%s", v.Outcome, v.Reason)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.Cause(schemachange.CauseRestartSwapCommitted), v.Cause)
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t).Phase)
}

// A checkpoint store an older engine created, which lacks a column this
// engine reads, cannot report the stopped run's row until a copy-and-swap
// run evolves it, and a native statement never does. While the run's shadow
// stands, the native
//
//	ALTER TABLE orders DROP COLUMN note
//
// is refused, not run over the stopped retype.
func TestRunRefusesAStatementWhileTheCheckpointStoreIsBehind(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)
	f.exec(t, `
		ALTER TABLE pgsprite.pgsprite_checkpoint
			DROP COLUMN statement`)

	drop, err := statement.ParseOne(f.inSchema(`
		ALTER TABLE {schema}.orders
			DROP COLUMN note`))
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.AllowDestructive = f.schema + ".orders"
	v, err := Run(t.Context(), f.pool, drop, opts)
	require.NoError(t, err)
	f.requireStoppedRunRefusal(t, v)
	assert.True(t, f.hasColumn(t, "orders", "note"), "nothing was executed")
}
