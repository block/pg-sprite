package copier_test

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// samplePositions reads the copier's Position every millisecond until the
// returned stop is called and checks each snapshot for consistency. A
// snapshot is consistent when a run that has cut nothing has landed nothing;
// when the lowest in-flight chunk starts just above the watermark, because a
// landed chunk there would have moved the watermark; when every in-flight
// chunk lies below the cut; and when a snapshot with nothing in flight has
// the watermark at the cut, because every claimed chunk has landed.
func samplePositions(t *testing.T, c *copier.Copier) (stop func()) {
	t.Helper()
	const sampleInterval = time.Millisecond
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				assertPositionConsistent(t, c.Position())
			}
		}
	})
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return stop
}

func assertPositionConsistent(t *testing.T, pos copier.Position) {
	t.Helper()
	if !pos.Cut.Valid() {
		assert.False(t, pos.Watermark.Valid(), "nothing can land before anything is cut: %+v", pos)
		assert.Empty(t, pos.InFlight, "nothing can be in flight before anything is cut: %+v", pos)
		return
	}
	if len(pos.InFlight) == 0 {
		assert.Equal(t, pos.Cut, pos.Watermark, "with nothing in flight every claimed chunk has landed: %+v", pos)
		return
	}
	frontier := int64(math.MinInt64)
	if pos.Watermark.Valid() {
		frontier = pos.Watermark.Value() + 1
	}
	assert.Equal(t, frontier, pos.InFlight[0].Lower(), "the lowest in-flight chunk starts just above the watermark: %+v", pos)
	for _, chunk := range pos.InFlight {
		assert.LessOrEqual(t, chunk.Upper(), pos.Cut.Value(), "an in-flight chunk lies at or below the cut: %+v", pos)
	}
}

// The copier copies every source row into the shadow with several workers
// and small chunks, finishes with the watermark at the largest key and
// nothing in flight, and runs once. Every Position sampled while it runs is
// consistent: the lowest in-flight chunk starts just above the watermark
// and every in-flight chunk lies at or below the cut, so no key between the
// two frontiers reads as landed before its chunk committed (CO-4).
func TestCopierCopiesTheWholeTable(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 5000
	target, lock, shadow := f.prepare(t, rows)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 3,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	stopSampling := samplePositions(t, c)
	require.NoError(t, c.Run(t.Context(), f.pool))
	stopSampling()

	f.assertConverged(t, shadow)
	pos := c.Position()
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), pos.Watermark, "a finished copy covers the whole key space")
	assert.Empty(t, pos.InFlight)
	assert.Equal(t, int64(rows), pos.RowsInserted)
	assert.Equal(t, copier.KeyLanded, pos.Classify(math.MaxInt64), "after the copy every key has landed")

	assert.ErrorIs(t, c.Run(t.Context(), f.pool), copier.ErrAlreadyRun)
}

// A row the applier writes into the shadow carries a fresher image than the
// copier's read; the copier leaves it alone (CO-4). The applier's write
// begins once the copy is under way and is committed while the chunk holding
// the key is in flight — the copier's insert waits on the uncommitted row
// and then finds it committed — which is the only way a row can be in the
// shadow ahead of the copier, since a copy that starts from nothing first
// clears the shadow and waits for every writer that preceded it.
func TestCopierNeverOverwritesAShadowRow(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	applied := f.pinShadowKeyAtFirstChunk(t, shadow, 30, 999)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       applied.clock,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	applied.wait(t)
	const inFlightDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return len(pos.InFlight) == 1 && pos.InFlight[0].Lower() == math.MinInt64
	}, inFlightDeadline, 20*time.Millisecond, "the first chunk, holding key 30, should be in flight")
	applied.commit(t)

	const stopDeadline = 30 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err)
	case <-time.After(stopDeadline):
		t.Fatalf("copy did not finish within %s of the applier's commit", stopDeadline)
	}

	var qty int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" WHERE id = 30").Scan(&qty))
	assert.Equal(t, int64(999), qty, "the pre-existing shadow row keeps its image")
	assert.Equal(t, int64(rows-1), c.Position().RowsInserted, "the skipped row is not counted as inserted")
	assert.Equal(t, int64(rows), f.count(t, shadow.ShadowTable(), "true"))
}

// Resuming after a watermark copies only the keys above it: the rows below
// are the checkpointed prefix an earlier run already landed and are left
// exactly as found. Whatever the earlier run's unlanded chunks left above
// the watermark is cleared first, in batches smaller than the tail, so the
// resumed copy reads every key above the watermark from the source (CO-4):
// a stale shadow row there would otherwise survive ON CONFLICT DO NOTHING.
func TestCopierResumesAfterTheWatermark(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 300)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	f.exec(t, "INSERT INTO "+shadowName+" (id, qty) VALUES (100, 999)")
	f.exec(t, "INSERT INTO "+shadowName+" (id, qty) SELECT n, 0 FROM generate_series(151, 250) AS n")

	c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(150), copier.Options{Workers: 2, Chunker: copier.ChunkerOptions{InitialRows: 40, MaxRows: 40}})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	assert.Equal(t, int64(1), f.count(t, shadow.ShadowTable(), "id <= 150"), "keys at or below the watermark are neither copied nor cleared")
	assert.Equal(t, int64(1), f.count(t, shadow.ShadowTable(), "id = 100 AND qty = 999"), "the landed prefix keeps its rows")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id > 150 AND qty = 0"), "stale rows above the watermark are gone")
	assert.Equal(t, int64(150), f.count(t, shadow.ShadowTable(), "id > 150 AND qty = id"), "every key above the watermark was read from the source")
	assert.Equal(t, int64(150), c.Position().RowsInserted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// A zero watermark means nothing landed, not that the shadow is empty: a run
// whose first chunk never landed may have landed chunks anywhere above it.
// The resumed copy therefore clears the whole shadow — down to the smallest
// key a row can carry — before its first chunk is cut, so a stale row for a
// key the applier had discarded changes for cannot survive (CO-4).
func TestCopierResumesFromTheZeroWatermark(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 300)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	f.exec(t, "INSERT INTO "+shadowName+" (id, qty) SELECT n, 0 FROM generate_series(101, 300) AS n")
	f.exec(t, "INSERT INTO "+shadowName+" (id, qty) VALUES ("+strconv.FormatInt(math.MinInt64, 10)+", 0)")

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 2, Chunker: copier.ChunkerOptions{InitialRows: 40, MaxRows: 40}})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "qty = 0"), "every stale row is gone, including the one at the smallest key")
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(300), c.Position().RowsInserted, "every source row was read again")
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// A cancelled copy returns only once every worker has exited (LK-3), and its
// Position still tells the truth: the cancelled chunk stays in flight, so
// its keys never read as landed, while every key at or below the watermark
// is in the shadow. A fresh copier resumed from that watermark clears the
// tail above it — including the chunks that had landed out of order, whose
// rows the applier may have discarded changes for as uncut — and converges
// on a source that changed in that tail meanwhile. One chunk is pinned
// mid-insert by an uncommitted shadow row for one of its keys, written once
// the copy is under way, so the cancellation lands on a statement that is
// genuinely in flight.
func TestCopierCancellationKeepsTheCancelledChunkInFlight(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 2000
	target, lock, shadow := f.prepare(t, rows)
	pin := f.pinShadowKeyAtFirstChunk(t, shadow, 1050, 0)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     4,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       pin.clock,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(ctx, f.pool) })
	t.Cleanup(wg.Wait)

	pin.wait(t)
	const pinnedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return pos.Cut == copier.NewWatermark(math.MaxInt64) && len(pos.InFlight) == 1
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
	require.Len(t, stopped.InFlight, 1, "the cancelled chunk stays in flight: it did not land")
	assert.Equal(t, int64(1001), stopped.InFlight[0].Lower())
	assert.Equal(t, int64(1100), stopped.InFlight[0].Upper())
	assert.Equal(t, copier.KeyInFlight, stopped.Classify(1050), "a key in the cancelled chunk never reads as landed")
	assert.Equal(t, copier.KeyLanded, stopped.Classify(1200))
	assert.Equal(t, copier.NewWatermark(1000), stopped.Watermark)
	assert.Equal(t, int64(1000), f.count(t, shadow.ShadowTable(), "id <= 1000"), "every key at or below the watermark landed")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id BETWEEN 1001 AND 1100"), "the cancelled chunk left nothing behind")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id = 1050"))
	pin.release(t)

	// A change the applier would have discarded as uncut, had it read this
	// Position: key 1500 is above the checkpointed watermark.
	f.exec(t, "UPDATE %s.orders SET qty = 999 WHERE id = 1500")
	resumed, err := copier.NewCopier(target, shadow, lock, stopped.Watermark, copier.Options{Workers: 2})
	require.NoError(t, err)
	require.NoError(t, resumed.Run(t.Context(), f.pool))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(1000), resumed.Position().RowsInserted, "everything above the watermark was cleared and copied again")
}

// Losing the table lock mid-copy cancels the statements in flight and Run
// reports the loss as an invariant violation naming what the session saw
// (LK-1); the chunk the loss interrupted stays in flight.
func TestCopierAbortsWhenTheLockIsLostMidCopy(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 2000)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target)
	require.NoError(t, buildLock.Release(t.Context()))
	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	pin := f.pinShadowKeyAtFirstChunk(t, shadow, 1050, 0)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     2,
		LockTimeout: 30 * time.Second,
		Clock:       pin.clock,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)
	pin.wait(t)

	const pinnedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return pos.Cut == copier.NewWatermark(math.MaxInt64) && len(pos.InFlight) == 1
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
	stopped := c.Position()
	require.Len(t, stopped.InFlight, 1, "the interrupted chunk did not land")
	assert.Equal(t, copier.KeyInFlight, stopped.Classify(1050))
	assert.Equal(t, copier.NewWatermark(1000), stopped.Watermark)
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
	// A relation that is gone fails the write transaction's own lock on it
	// before its identity is checked. The copy resumes after a watermark so
	// the first guarded transaction — the resume clear — runs before the
	// chunker's boundary query would report the missing table on its own.
	t.Run("source dropped", func(t *testing.T) {
		f := newCopierFixture(t)
		target, lock, shadow := f.prepare(t, 100)
		f.exec(t, "DROP TABLE %s.orders")

		c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(50), copier.Options{Workers: 1})
		require.NoError(t, err)
		err = c.Run(t.Context(), f.pool)
		require.ErrorIs(t, err, copier.ErrInvariantViolation)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server's refusal to lock a missing relation is the cause")
		assert.Equal(t, "42P01", pgErr.Code, "undefined_table")
		assert.Contains(t, err.Error(), "(ST-6)")
		assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"))
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
	require.NoError(t, rival.Release(t.Context()))

	lost := f.lostLock(t, "orders")
	_, err = copier.NewCopier(target, shadow, lost, copier.Watermark{}, copier.Options{})
	require.ErrorIs(t, err, copier.ErrInvariantViolation, "a session that already reported loss")
	assert.ErrorIs(t, err, lost.Err(), "the refusal names what the session saw")
	assert.Contains(t, err.Error(), "(LK-1)")
}

// lostLock acquires the table lock with a short keepalive, terminates the
// session's backend, and waits until the session itself has reported the
// loss, so Err is set before the copier ever sees the session.
func (f copierFixture) lostLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, dbconn.WithTableLockKeepalive(200*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { assert.Error(t, lock.Release(context.WithoutCancel(t.Context()))) })
	f.terminateBackend(t, lock.BackendPID())
	const lockLossDeadline = 30 * time.Second
	select {
	case <-lock.Done():
	case <-time.After(lockLossDeadline):
		t.Fatalf("lock session did not report loss within %s", lockLossDeadline)
	}
	require.Error(t, lock.Err())
	return lock
}

// hookedClock is a Clock whose first reading runs a hook. The copier reads
// the clock once per chunk, after the chunk is claimed and before its
// transaction begins, so the hook lands after the resume clear's guarded
// transaction has committed and before the first chunk's begins.
type hookedClock struct {
	once sync.Once
	hook func()
}

func (c *hookedClock) Now() time.Time {
	c.once.Do(c.hook)
	return time.Now()
}

// shadowPin is a row inserted into the shadow by a transaction left open,
// standing in for an applier write that is under way while the copier runs:
// the copier's insert of the chunk holding the key waits on it, so that
// chunk stays in flight until the pin is committed or rolled back.
type shadowPin struct {
	// clock places the pin; the copier under test takes it as Options.Clock.
	clock *hookedClock
	// pinned is closed once tx and err are set.
	pinned chan struct{}
	tx     pgx.Tx
	err    error
	done   sync.Once
}

// pinShadowKeyAtFirstChunk arranges for key to be pinned in the shadow with
// image qty at the copier's first clock reading — after the resume clear and
// the first claim, before the first chunk transaction — so the pin is a
// concurrent writer the copy meets, not a straggler the clear's fence waits
// for. Callers wait before touching the pin and end it with commit or
// release; a pin still open at cleanup is rolled back.
func (f copierFixture) pinShadowKeyAtFirstChunk(t *testing.T, shadow schemachange.BuiltShadow, key, qty int64) *shadowPin {
	t.Helper()
	pin := &shadowPin{pinned: make(chan struct{})}
	pin.clock = &hookedClock{hook: func() {
		defer close(pin.pinned)
		// The hook runs on a copier worker; failures are reported through
		// err and checked by wait on the test goroutine.
		tx, err := f.pool.Begin(t.Context())
		if err != nil {
			pin.err = fmt.Errorf("begin the pin: %w", err)
			return
		}
		if _, err := tx.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" (id, qty) VALUES ($1, $2)", key, qty); err != nil {
			pin.err = fmt.Errorf("pin key %d: %w", key, err)
			// Redundant safety closer: the failed insert aborted the
			// transaction, and the server ends it with the connection.
			_ = tx.Rollback(context.WithoutCancel(t.Context()))
			return
		}
		pin.tx = tx
	}}
	t.Cleanup(func() {
		select {
		case <-pin.pinned:
			pin.release(t)
		default:
		}
	})
	return pin
}

// wait blocks until the pin is in place and fails the test if placing it
// did not succeed.
func (p *shadowPin) wait(t *testing.T) {
	t.Helper()
	const pinDeadline = 15 * time.Second
	select {
	case <-p.pinned:
	case <-time.After(pinDeadline):
		t.Fatalf("the copier did not reach its first chunk within %s", pinDeadline)
	}
	require.NoError(t, p.err)
}

// release rolls the pin back, once.
func (p *shadowPin) release(t *testing.T) {
	t.Helper()
	p.done.Do(func() {
		if p.tx != nil {
			assert.NoError(t, p.tx.Rollback(context.WithoutCancel(t.Context())))
		}
	})
}

// commit publishes the pinned row, once.
func (p *shadowPin) commit(t *testing.T) {
	t.Helper()
	p.done.Do(func() { require.NoError(t, p.tx.Commit(t.Context())) })
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
