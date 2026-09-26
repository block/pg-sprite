package copier_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// copierFixture is a throwaway schema on a superuser pool with the real
// shadow builder in front of the copier, so every copy below writes into a
// shadow the builder proved. The superuser is a SET-usable member of every
// role, so the copy-and-swap proof is minted without provisioning.
type copierFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

func newCopierFixture(t *testing.T) copierFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return copierFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f copierFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// createOrders creates the orders table every copy below reads, holding
// rows keys 1..rows with qty = key and a note; the note is the column the
// shadow drops, so the copy column list is narrower than the source.
func (f copierFixture) createOrders(t *testing.T, rows int64) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.orders (id, qty, note)
		SELECT n, n, 'order ' || n FROM generate_series(1, %d) AS n`, rows))
}

// prove mints the copy-and-swap proof for table.
func (f copierFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
	require.NoError(t, err)
	return target
}

// lock acquires the per-table lock the copier requires and releases it when
// the test ends.
func (f copierFixture) lock(t *testing.T, table string, options ...dbconn.TableLockOption) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A test that deliberately loses the lock has already seen Release's
		// invariant error through Err; a clean test releases cleanly.
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	return lock
}

// build runs the shadow builder for table under lock, dropping the note
// column so the shadow has fewer columns than the source.
func (f copierFixture) build(t *testing.T, lock *dbconn.TableLockSession, target preflight.CopySwapTarget) schemachange.BuiltShadow {
	t.Helper()
	alter, err := statement.ParseOne(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN note`, pgx.Identifier{f.schema, target.Table()}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	return shadow
}

// prepare creates, proves, locks, and builds the shadow of an orders table
// holding rows rows.
func (f copierFixture) prepare(t *testing.T, rows int64) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	f.createOrders(t, rows)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	return target, lock, f.build(t, lock, target)
}

func (f copierFixture) count(t *testing.T, table, where string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, table}.Sanitize()+" WHERE "+where).Scan(&n))
	return n
}

// assertConverged compares the source with its shadow on the columns the
// shadow kept.
func (f copierFixture) assertConverged(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
}

// The copier copies every source row into the shadow with several workers
// and small chunks, finishes with the watermark at the largest key and
// nothing in flight, and runs once.
func TestCopierCopiesTheWholeTable(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 5000
	target, lock, shadow := f.prepare(t, rows)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 3,
		Chunker: copier.ChunkerOptions{InitialRows: 700, MaxRows: 700},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	f.assertConverged(t, shadow)
	pos := c.Position()
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), pos.Watermark, "a finished copy covers the whole key space")
	assert.Empty(t, pos.InFlight)
	assert.Equal(t, int64(rows), pos.RowsInserted)
	assert.Equal(t, copier.KeyLanded, pos.Classify(math.MaxInt64), "after the copy every key has landed")

	assert.ErrorIs(t, c.Run(t.Context(), f.pool), copier.ErrAlreadyRun)
}

// A row the applier already wrote into the shadow carries a fresher image
// than the copier's read; the copier leaves it alone (CO-4).
func TestCopierNeverOverwritesAShadowRow(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, `INSERT INTO %s.`+pgx.Identifier{shadow.ShadowTable()}.Sanitize()+` (id, qty) VALUES (30, 999)`)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	var qty int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" WHERE id = 30").Scan(&qty))
	assert.Equal(t, int64(999), qty, "the pre-existing shadow row keeps its image")
	assert.Equal(t, int64(rows-1), c.Position().RowsInserted, "the skipped row is not counted as inserted")
	assert.Equal(t, int64(rows), f.count(t, shadow.ShadowTable(), "true"))
}

// Resuming after a watermark copies only the keys above it: the rows below
// are the checkpointed prefix an earlier run already landed.
func TestCopierResumesAfterTheWatermark(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 300)

	c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(150), copier.Options{Workers: 2, Chunker: copier.ChunkerOptions{InitialRows: 40, MaxRows: 40}})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id <= 150"), "keys at or below the watermark are not copied")
	assert.Equal(t, int64(150), f.count(t, shadow.ShadowTable(), "id > 150"))
	assert.Equal(t, int64(150), c.Position().RowsInserted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// A cancelled copy returns only once every chunk transaction has ended
// (LK-3): nothing is in flight, every key at or below the watermark is in
// the shadow, and a fresh copier resumed from that watermark converges.
// One chunk is pinned mid-insert by an uncommitted shadow row for one of its
// keys, so the cancellation lands on a statement that is genuinely in flight
// while the chunks around it have landed out of order.
func TestCopierCancellationLeavesNothingInFlight(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 2000
	target, lock, shadow := f.prepare(t, rows)
	release := f.pinShadowKey(t, shadow, 1050)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     4,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(ctx, f.pool) })
	t.Cleanup(wg.Wait)

	const pinnedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return pos.CutValid && pos.Cut == math.MaxInt64 && len(pos.InFlight) == 1
	}, pinnedDeadline, 20*time.Millisecond, "every chunk but the pinned one should land")
	pinned := c.Position()
	assert.Equal(t, copier.NewWatermark(1000), pinned.Watermark, "the watermark stops below the pinned chunk")
	assert.Equal(t, copier.KeyInFlight, pinned.Classify(1050))
	assert.Equal(t, copier.KeyLanded, pinned.Classify(1200), "a chunk landed above the pinned one is landed, not uncut")
	cancel()

	const stopDeadline = 15 * time.Second
	select {
	case err := <-results:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(stopDeadline):
		t.Fatalf("copy did not stop within %s of cancellation", stopDeadline)
	}
	stopped := c.Position()
	assert.Empty(t, stopped.InFlight, "Run returns only after every chunk transaction has ended")
	assert.Equal(t, copier.NewWatermark(1000), stopped.Watermark)
	assert.Equal(t, int64(1000), f.count(t, shadow.ShadowTable(), "id <= 1000"), "every key at or below the watermark landed")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id BETWEEN 1001 AND 1100"), "the cancelled chunk left nothing behind")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id = 1050"))
	release()

	resumed, err := copier.NewCopier(target, shadow, lock, stopped.Watermark, copier.Options{Workers: 2})
	require.NoError(t, err)
	require.NoError(t, resumed.Run(t.Context(), f.pool))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(100), resumed.Position().RowsInserted, "only the cancelled chunk's rows were missing")
}

// Losing the table lock mid-copy cancels the statements in flight and Run
// reports the loss as an invariant violation naming what the session saw
// (LK-1); the copy leaves nothing in flight.
func TestCopierAbortsWhenTheLockIsLostMidCopy(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 2000)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target)
	require.NoError(t, buildLock.Release(t.Context()))
	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	release := f.pinShadowKey(t, shadow, 1050)
	defer release()

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     2,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	const pinnedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return pos.CutValid && pos.Cut == math.MaxInt64 && len(pos.InFlight) == 1
	}, pinnedDeadline, 20*time.Millisecond, "every chunk but the pinned one should land")
	f.terminateBackend(t, lock.BackendPID())

	const lockLossDeadline = 15 * time.Second
	select {
	case err := <-results:
		assert.ErrorIs(t, err, copier.ErrInvariantViolation)
		assert.ErrorIs(t, err, lock.Err(), "the loss the session reported is the cause")
	case <-time.After(lockLossDeadline):
		t.Fatalf("copy did not abort within %s of lock loss", lockLossDeadline)
	}
	assert.Empty(t, c.Position().InFlight)
}

// The copy refuses to write when the shadow, or the source, has been
// replaced by another relation of the same name since the proofs were
// minted (ST-6): the same-shaped impostor receives no rows.
func TestCopierRefusesReplacedRelations(t *testing.T) {
	t.Run("shadow replaced", func(t *testing.T) {
		f := newCopierFixture(t)
		target, lock, shadow := f.prepare(t, 100)
		shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
		f.exec(t, "DROP TABLE "+shadowName)
		f.exec(t, "CREATE TABLE "+shadowName+" (id bigint PRIMARY KEY, qty integer NOT NULL)")

		c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
		require.NoError(t, err)
		err = c.Run(t.Context(), f.pool)
		require.ErrorIs(t, err, copier.ErrInvariantViolation)
		assert.Contains(t, err.Error(), "(ST-6): shadow")
		assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "the impostor receives nothing")
	})
	t.Run("source replaced", func(t *testing.T) {
		f := newCopierFixture(t)
		target, lock, shadow := f.prepare(t, 100)
		f.exec(t, "DROP TABLE %s.orders")
		f.exec(t, "CREATE TABLE %s.orders (id bigint PRIMARY KEY, qty integer NOT NULL, note text)")
		f.exec(t, "INSERT INTO %s.orders (id, qty) VALUES (1, 1)")

		c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
		require.NoError(t, err)
		err = c.Run(t.Context(), f.pool)
		require.ErrorIs(t, err, copier.ErrInvariantViolation)
		assert.Contains(t, err.Error(), "(ST-6): source")
		assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "nothing is copied from a table nobody proved")
	})
}

// The copier trusts the lock session only as far as the server confirms it
// from the writing connection (LK-1): a session whose backend is gone, or
// whose lock another backend now holds, copies nothing. A session for a
// different table, or one that already reported loss, is refused before any
// connection is opened.
func TestCopierRefusesAnUnconfirmedLock(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 100)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target)
	require.NoError(t, buildLock.Release(t.Context()))

	f.exec(t, `CREATE TABLE %s.other (id bigint PRIMARY KEY)`)
	_, err := copier.NewCopier(target, shadow, f.lock(t, "other"), copier.Watermark{}, copier.Options{})
	require.ErrorIs(t, err, copier.ErrInvariantViolation, "a lock for another table")

	stale := f.goneLock(t, "orders")
	c, err := copier.NewCopier(target, shadow, stale, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation, "a gone lock session")
	assert.ErrorIs(t, err, dbconn.ErrTableLockNotHeld)
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"))

	rival := f.lock(t, "orders")
	c, err = copier.NewCopier(target, shadow, stale, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.ErrorIs(t, err, copier.ErrInvariantViolation, "a lock held by another backend")
	var heldErr *dbconn.TableLockHeldError
	require.ErrorAs(t, err, &heldErr)
	assert.Equal(t, rival.BackendPID(), heldErr.Holder.PID)
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"))
	assert.NoError(t, rival.Err(), "the rival's lock is untouched")
}

// pinShadowKey inserts key into the shadow in a transaction it leaves open,
// so the copier's insert of the chunk holding key waits on that transaction
// and the chunk stays in flight until the returned release rolls it back.
func (f copierFixture) pinShadowKey(t *testing.T, shadow schemachange.BuiltShadow, key int64) (release func()) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" (id, qty) VALUES ($1, 0)", key)
	require.NoError(t, err)
	var once sync.Once
	release = func() {
		once.Do(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	}
	t.Cleanup(release)
	return release
}

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it: the default keepalive is long enough that only an
// in-transaction confirmation can catch the loss.
func (f copierFixture) goneLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		const lockLossDeadline = 30 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
	})
	f.terminateBackend(t, lock.BackendPID())
	require.NoError(t, lock.Err(), "the keepalive has not yet noticed the loss; the in-transaction check must")
	return lock
}

func (f copierFixture) terminateBackend(t *testing.T, pid uint32) {
	t.Helper()
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive))
		return !alive
	}, backendExitDeadline, 50*time.Millisecond, "terminated backend should leave pg_stat_activity")
}
