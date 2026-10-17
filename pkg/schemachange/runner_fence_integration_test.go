package schemachange_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests below pin the write fence: the statement trigger a quiesced
// run puts on its source so that a write reaching the table while the run
// is in progress — or stopped between two saves — is refused by the
// server rather than copied past (CO-1).

// writeStatements are the four kinds of statement the fence refuses, each
// against the fixture's orders table.
func (f runnerFixture) writeStatements(table string) map[string]string {
	name := pgx.Identifier{f.schema, table}.Sanitize()
	return map[string]string{
		"insert":   "INSERT INTO " + name + " (qty) VALUES (1)",
		"update":   "UPDATE " + name + " SET qty = qty WHERE id = 1",
		"delete":   "DELETE FROM " + name + " WHERE id = -1",
		"truncate": "TRUNCATE " + name,
	}
}

// assertWriteFenced checks that every kind of write to table is refused
// with the fence's SQLSTATE, and that the refusal leaves the row count as
// it was.
func (f runnerFixture) assertWriteFenced(t *testing.T, table string) {
	t.Helper()
	before := f.rowCount(t, table)
	for kind, sql := range f.writeStatements(table) {
		_, err := f.pool.Exec(t.Context(), sql)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "%s against the fenced table should be refused by the server", kind)
		assert.Equal(t, schemachange.WriteFenceSQLState, pgErr.Code, "%s is refused with the fence's SQLSTATE", kind)
	}
	assert.Equal(t, before, f.rowCount(t, table), "a refused write changes nothing")
}

// assertWritable checks that table takes an insert and an update again,
// and undoes them so the table reads as before.
func (f runnerFixture) assertWritable(t *testing.T, table string) {
	t.Helper()
	name := pgx.Identifier{f.schema, table}.Sanitize()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) }()
	_, err = tx.Exec(t.Context(), "INSERT INTO "+name+" (qty) VALUES (1)")
	assert.NoError(t, err, "the table takes an insert")
	_, err = tx.Exec(t.Context(), "UPDATE "+name+" SET qty = qty WHERE id = 1")
	assert.NoError(t, err, "the table takes an update")
}

// hasFence reports whether table carries the fence trigger.
func (f runnerFixture) hasFence(t *testing.T, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_trigger tg
			JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2 AND tg.tgname = $3 AND NOT tg.tgisinternal)`,
		f.schema, table, schemachange.WriteFenceTrigger).Scan(&exists))
	return exists
}

// While a run is in progress the source refuses every write with the
// fence's SQLSTATE: the hook that fires after the cutover save, with the
// copy and the verification pass behind it and the swap still ahead, is
// the latest point a writer could reach the source, and it is turned
// away there. The run still completes, and the live table — the shadow
// swapped in — takes writes again, so the fence did not ride to it.
func TestRunnerFencesTheSourceWhileItRuns(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	fencedAtCutover := false
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCutover {
			f.assertWriteFenced(t, "orders")
			fencedAtCutover = true
		}
		return nil
	}}

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, opts)
	require.NoError(t, err)

	assert.True(t, fencedAtCutover, "the hook saw the cutover save")
	assert.False(t, f.hasFence(t, "orders"), "the live table carries no fence")
	f.assertWritable(t, "orders")
}

// Under KeepOld the fence rode the swap's rename to the retained source,
// and the run takes it off before it is done: a table kept for a rollback
// window has to be writable the moment it is renamed back.
func TestRunnerUnfencesTheOldTableItKeeps(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{KeepOld: true})
	require.NoError(t, err)

	old := schemachange.OldName(f.schema, "orders")
	assert.False(t, f.hasFence(t, old), "the retained source carries no fence")
	f.assertWritable(t, old)
	f.assertWritable(t, "orders")
}

// The fence outlives the run that installed it: a run stopped after its
// first save leaves the source refusing writes, since the copy it made so
// far is only good while nothing changes under it. The resumed run
// installs over the fence it finds and completes.
func TestRunnerLeavesTheFenceOnAStoppedRun(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCopying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)

	assert.True(t, f.hasFence(t, "orders"), "the stopped run leaves the fence in place")
	f.assertWriteFenced(t, "orders")
	f.assertResumedToDone(t, alter, schemachange.RunOptions{})
	f.assertWritable(t, "orders")
}

// Abandoning a stopped run is the operator's three steps under the
// table's lock: drop the shadow, take the fence off the source, delete
// the row. After them the source takes writes again, and the next Run
// starts fresh.
func TestDropWriteFenceRestoresWritesToAnAbandonedSource(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCopying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	row := f.inspectRow(t, "orders")
	f.assertWriteFenced(t, "orders")

	lock := f.lock(t, "orders")
	target := f.prove(t, "orders")
	require.NoError(t, schemachange.DropShadow(t.Context(), f.pool, lock, target, schemachange.Options{}))
	require.NoError(t, schemachange.DropWriteFence(t.Context(), f.pool, lock, target, schemachange.Options{}))
	require.NoError(t, f.store.Delete(t.Context(), lock, f.schema, "orders", row.Identity()))
	require.NoError(t, lock.Release(t.Context()))

	assert.False(t, f.hasFence(t, "orders"))
	f.assertWritable(t, "orders")
	again := f.lock(t, "orders")
	assert.NoError(t, schemachange.DropWriteFence(t.Context(), f.pool, again, target, schemachange.Options{}), "a source without the fence is left as it is")
	require.NoError(t, again.Release(t.Context()))

	result, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
}

// DropWriteFence is for the source of a run; a relation of another kind
// under the source's name, or none, is not it. A view under the name is
// refused as a foreign relation (ST-5) and nothing under it is
// ErrSourceNotFound, before any DROP is attempted.
func TestDropWriteFenceRefusesWhatIsNotTheSource(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")

	f.exec(t, `ALTER TABLE %s.orders RENAME TO orders_moved`)
	err := schemachange.DropWriteFence(t.Context(), f.pool, lock, target, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrSourceNotFound)

	f.exec(t, `CREATE VIEW %s.orders AS SELECT * FROM %s.orders_moved`)
	err = schemachange.DropWriteFence(t.Context(), f.pool, lock, target, schemachange.Options{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseForeignRelation, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, "orders"), "the refused drop touches nothing")
}

// A fault at the first save — the row written right after the shadow is
// built and the source fenced — leaves neither behind: a shadow nothing
// records would refuse the next run as shadow-unrecorded, so the run that
// could not record it drops it and takes its fence off. The next Run
// finds no row, no shadow, and a writable source, and starts fresh.
func TestRunnerDropsTheShadowWhenItsFirstSaveFails(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	shadow := schemachange.ShadowName(f.schema, "orders")

	// The store's clock is read once per save, while the row is being
	// written; a clock that cancels the run there fails that save and
	// nothing before it.
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{Clock: &cancelClock{cancel: cancel}})
	require.NoError(t, err)
	r, err := schemachange.NewRunner(f.cfg, store, f.runOptions(schemachange.RunOptions{}))
	require.NoError(t, err)

	_, err = r.Run(runCtx, f.pool, f.prove(t, "orders"), f.alter(t, alter))
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, f.relationExists(t, shadow), "the shadow the failed save could not record is dropped")
	assert.False(t, f.hasFence(t, "orders"), "the fence that went on with the shadow comes off with it")
	f.assertWritable(t, "orders")
	_, err = f.store.Inspect(t.Context(), f.schema, "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "no row was written")

	result, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
}

// fenceEnabled reads pg_trigger.tgenabled of the fence on table: "A" fires
// in every session, "O" only in origin sessions, "D" never.
func (f runnerFixture) fenceEnabled(t *testing.T, table string) string {
	t.Helper()
	var enabled string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT tg.tgenabled::text FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND tg.tgname = $3 AND NOT tg.tgisinternal`,
		f.schema, table, schemachange.WriteFenceTrigger).Scan(&enabled))
	return enabled
}

// assertReplicaWritesFenced checks that a session running under
// session_replication_role = replica, which skips every trigger not enabled
// ALWAYS, is refused by the fence on every kind of write to table.
func (f runnerFixture) assertReplicaWritesFenced(t *testing.T, table string) {
	t.Helper()
	conn, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(t.Context(), "SET session_replication_role = replica")
	require.NoError(t, err, "the test role may set session_replication_role")
	defer func() {
		_, err := conn.Exec(context.WithoutCancel(t.Context()), "RESET session_replication_role")
		assert.NoError(t, err)
	}()
	before := f.rowCount(t, table)
	for kind, sql := range f.writeStatements(table) {
		_, err := conn.Exec(t.Context(), sql)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "%s from a replica-role session should be refused by the server", kind)
		assert.Equal(t, schemachange.WriteFenceSQLState, pgErr.Code, "%s is refused with the fence's SQLSTATE", kind)
	}
	assert.Equal(t, before, f.rowCount(t, table), "a refused write changes nothing")
}

// A session running under session_replication_role = replica fires no
// ordinary trigger, so a fence enabled for origin sessions only would let
// its write through and the swap would drop the row. The fence is enabled
// ALWAYS, so every kind of write from such a session is refused at the
// latest point a writer could reach the source, after the verification pass
// and with the swap still ahead, and the run completes.
func TestRunnerFenceRefusesASessionInReplicaRole(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	checked := false
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCutover {
			assert.Equal(t, "A", f.fenceEnabled(t, "orders"), "the fence fires in every session")
			f.assertReplicaWritesFenced(t, "orders")
			checked = true
		}
		return nil
	}}

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, opts)
	require.NoError(t, err)

	assert.True(t, checked, "the hook saw the cutover save")
}

// The swap of a quiesced proof is gated on the fence still being there. A
// fence disabled after the verification pass, with the swap ahead, no longer
// keeps the source quiet, so the swap refuses as cutover-unfenced and leaves
// the source in place. The row stays in cutover, and the next Run installs
// the fence again, verifies again, and completes.
func TestRunnerRefusesTheSwapWhenTheFenceWasDisabled(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCutover {
			f.exec(t, `ALTER TABLE %s.orders DISABLE TRIGGER pgsprite_write_fence`)
		}
		return nil
	}}

	_, err := f.run(t, "orders", alter, opts)

	require.Error(t, err)
	assert.Equal(t, schemachange.CauseCutoverUnfenced, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrCutoverRolledBack)
	assert.True(t, f.hasColumn(t, "orders", "note"), "the source was not swapped out")
	assert.True(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "the shadow is still in place")
	assert.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase)
	assert.Equal(t, "D", f.fenceEnabled(t, "orders"), "the refusal does not touch the fence")

	f.assertResumedToDone(t, alter, schemachange.RunOptions{})
	f.assertWritable(t, "orders")
}

// A fence dropped after the verification pass is refused the same way: the
// trigger the proof relies on is gone, so the swap does not proceed, and the
// resumed run installs it again.
func TestRunnerRefusesTheSwapWhenTheFenceWasDropped(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCutover {
			f.exec(t, `DROP TRIGGER pgsprite_write_fence ON %s.orders`)
		}
		return nil
	}}

	_, err := f.run(t, "orders", alter, opts)

	require.Error(t, err)
	assert.Equal(t, schemachange.CauseCutoverUnfenced, schemachange.RefusalCauseOf(err))
	assert.True(t, f.hasColumn(t, "orders", "note"), "the source was not swapped out")
	assert.False(t, f.hasFence(t, "orders"))

	f.assertResumedToDone(t, alter, schemachange.RunOptions{})
	f.assertWritable(t, "orders")
}

// A fence an earlier engine left enabled for origin sessions only is not
// refused for good: the resumed run installs over it, which enables it
// ALWAYS, so the swap finds the fence it requires.
func TestRunnerResumeUpgradesAnOriginFenceToAlways(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCopying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	f.exec(t, `ALTER TABLE %s.orders ENABLE TRIGGER pgsprite_write_fence`)
	require.Equal(t, "O", f.fenceEnabled(t, "orders"), "the stopped run's fence is origin-only")

	upgraded := false
	opts := schemachange.RunOptions{AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase == checkpoint.PhaseCutover {
			assert.Equal(t, "A", f.fenceEnabled(t, "orders"), "the resume enabled the fence ALWAYS")
			upgraded = true
		}
		return nil
	}}
	f.assertResumedToDone(t, alter, opts)

	assert.True(t, upgraded, "the hook saw the cutover save")
}

// cancelClock cancels a context whenever it is read, and tells the wall
// clock's time.
type cancelClock struct {
	cancel context.CancelFunc
}

func (c *cancelClock) Now() time.Time {
	c.cancel()
	return time.Now()
}
