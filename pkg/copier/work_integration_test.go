package copier_test

import (
	"context"
	"fmt"
	"math"
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
	"github.com/block/pg-sprite/pkg/progress"
)

// copyTracker is a tracker mid copy step, the way the orchestrator will
// hand one to the copier: the step is the caller's, the counters the
// copier's.
func copyTracker(t *testing.T) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationCopy)
	tracker.StartStep(1, progress.OperationCopy, "")
	return tracker
}

// tableSize is pg_table_size of table, the figure the copier reports as
// bytes for it.
func (f copierFixture) tableSize(t *testing.T, table string) uint64 {
	t.Helper()
	var size int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT pg_table_size($1::regclass)", pgx.Identifier{f.schema, table}.Sanitize()).Scan(&size))
	return uint64(size)
}

// While the copy runs, a poll of the tracker carries the copier's work:
// rows from the chunks that committed, the source's catalog row count, and
// the two tables' sizes measured at the poll. The poll lands while one
// chunk is pinned mid-insert and every other chunk has landed, so the
// counters have one right answer. Once Run returns the tracker no longer
// asks the copier, and the copier, holding no connection, reports its rows
// without sizes.
func TestCopierReportsItsWorkWhileItRuns(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 2000
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, "ANALYZE %s.orders")
	pin := f.pinShadowKeyAtFirstChunk(t, shadow, 1050, 0)
	tracker := copyTracker(t)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     4,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       pin.clock,
		Tracker:     tracker,
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

	mid, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, mid.Detail.Work, "a poll during the copy carries the copier's work")
	assert.Equal(t, progress.Work{
		RowsCopied:  rows - 100,
		RowsTotal:   rows,
		BytesCopied: f.tableSize(t, shadow.ShadowTable()),
		BytesTotal:  f.tableSize(t, shadow.SourceTable()),
	}, *mid.Detail.Work, "the pinned chunk's 100 rows have not committed; nothing writes either table while the pin holds")
	assert.Positive(t, mid.Detail.Work.BytesCopied, "the landed chunks occupy shadow pages")
	assert.Positive(t, mid.Detail.Work.BytesTotal)

	pin.release(t)
	const finishDeadline = 15 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err)
	case <-time.After(finishDeadline):
		t.Fatalf("copy did not finish within %s of releasing the pin", finishDeadline)
	}
	f.assertConverged(t, shadow)

	after, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a finished copier is no longer the tracker's work source")
	direct, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{RowsCopied: rows, RowsTotal: rows}, direct, "the ledger's rows survive Run; the sizes need a connection the copier no longer holds")
}

// The copy's measurements resolve in pg_catalog like every proof (CO-9).
// The pool a library caller hands Run may put a user schema ahead of
// pg_catalog on search_path, and that schema here offers an impostor
// pg_class whose reltuples is wrong for the source, an empty impostor
// pg_namespace, and a pg_table_size(oid) whose exact signature would win
// function resolution over pg_catalog.pg_table_size(regclass) whatever the
// path order. The poll must report the real count and the real sizes and
// never call the impostor; the chunk guard's identity check, which resolves
// both relations by name in the same session, must find them in the real
// catalog rather than refuse a copy the empty impostor cannot see.
func TestCopierWorkResistsCatalogShadowing(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 2000
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, "ANALYZE %s.orders")
	f.exec(t, fmt.Sprintf(`
		CREATE TABLE %%s.pg_class (
			oid oid,
			relname name,
			relnamespace oid,
			reltuples real
		);
		INSERT INTO %%s.pg_class (oid, relname, relnamespace, reltuples)
		VALUES (%d, 'orders', 0, 424242);
		CREATE TABLE %%s.pg_namespace (
			oid oid,
			nspname name
		);
		CREATE TABLE %%s.impostor_calls (
			n integer
		);
		CREATE FUNCTION %%s.pg_table_size(oid) RETURNS bigint LANGUAGE sql
			AS 'INSERT INTO %%s.impostor_calls VALUES (1); SELECT 424242::bigint'`, shadow.SourceOID()))
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)
	pin := f.pinShadowKeyAtFirstChunk(t, shadow, 1050, 0)
	tracker := copyTracker(t)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     4,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       pin.clock,
		Tracker:     tracker,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), shadowing) })
	t.Cleanup(wg.Wait)

	pin.wait(t)
	const pinnedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		pos := c.Position()
		return pos.Cut == copier.NewWatermark(math.MaxInt64) && len(pos.InFlight) == 1
	}, pinnedDeadline, 20*time.Millisecond, "every chunk but the pinned one should land")

	mid, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, mid.Detail.Work)
	assert.Equal(t, progress.Work{
		RowsCopied:  rows - 100,
		RowsTotal:   rows,
		BytesCopied: f.tableSize(t, shadow.ShadowTable()),
		BytesTotal:  f.tableSize(t, shadow.SourceTable()),
	}, *mid.Detail.Work, "the counters come from the real catalog, not the impostors")
	assert.Equal(t, int64(0), f.count(t, "impostor_calls", "true"), "the poll must not call a function from the session's search_path")

	pin.release(t)
	const finishDeadline = 15 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err, "the chunk guard resolves both relations in the real catalog")
	case <-time.After(finishDeadline):
		t.Fatalf("copy did not finish within %s of releasing the pin", finishDeadline)
	}
	f.assertConverged(t, shadow)
}

// The copier is the tracker's work source for the whole of Run, including
// the resume clear: on a large resumed shadow the clear is the long quiet
// stretch, and a poll during it must still show the step as the copy. The
// clear's first batch is held behind a straggling chunk transaction of the
// earlier run, and the poll lands while it waits: no rows of this run have
// landed, the source's count and both sizes are measured.
func TestCopierReportsItsWorkDuringTheResumeClear(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, "ANALYZE %s.orders")
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	// The earlier run landed keys 1..150 and checkpointed watermark 150.
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= 150`, pgx.Identifier{shadow.ShadowTable()}.Sanitize()))

	straggler, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	// Redundant safety closer: the test commits, after which Rollback
	// returns the guaranteed ErrTxClosed.
	t.Cleanup(func() { _ = straggler.Rollback(context.WithoutCancel(t.Context())) })
	_, err = straggler.Exec(t.Context(), "INSERT INTO "+shadowName+" (id, qty) VALUES (200, 0)")
	require.NoError(t, err)
	tracker := copyTracker(t)

	c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(150), copier.Options{
		Workers:     1,
		LockTimeout: 30 * time.Second,
		Chunker:     copier.ChunkerOptions{InitialRows: 40, MaxRows: 40},
		Tracker:     tracker,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	const fenceDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.waitsForShareLock(t, shadow.ShadowOID())
	}, fenceDeadline, 20*time.Millisecond, "the first clear batch should wait behind the straggler's lock")

	during, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	require.NotNil(t, during.Detail.Work, "a poll during the clear carries the copier's work")
	assert.Equal(t, progress.Work{
		RowsCopied:  0,
		RowsTotal:   rows,
		BytesCopied: f.tableSize(t, shadow.ShadowTable()),
		BytesTotal:  f.tableSize(t, shadow.SourceTable()),
	}, *during.Detail.Work, "nothing of this run has landed while the clear waits; the earlier run's rows occupy the shadow's pages")

	require.NoError(t, straggler.Commit(t.Context()))
	const stopDeadline = 30 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err)
	case <-time.After(stopDeadline):
		t.Fatalf("Run did not return within %s of the straggler committing", stopDeadline)
	}
	f.assertConverged(t, shadow)
}

// A poll bounds itself whatever pool the caller built: the size read runs
// under the copier's own lock timeout, not the pool's session defaults,
// which a pool built outside pkg/dbconn does not have. The worker is parked
// at its first clock reading — registered with the tracker, no chunk
// transaction open — while a reader holds ACCESS SHARE on the source and an
// application queues an ACCESS EXCLUSIVE request behind it. The poll's own
// ACCESS SHARE queues behind that request and must give up at the copier's
// lock timeout rather than wait out the application. Once the application
// gives up and the worker is released, the copy completes.
func TestCopierWorkIsBoundedOnACallerBuiltPool(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	unbounded, err := pgxpool.New(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(unbounded.Close)
	parked, resume := make(chan struct{}), make(chan struct{})
	park := &hookedClock{hook: func() {
		close(parked)
		<-resume
	}}
	tracker := copyTracker(t)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     1,
		LockTimeout: 500 * time.Millisecond,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       park,
		Tracker:     tracker,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), unbounded) })
	t.Cleanup(wg.Wait)
	const parkDeadline = 15 * time.Second
	select {
	case <-parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the copier did not reach its first chunk within %s", parkDeadline)
	}

	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()
	reader, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), "LOCK TABLE "+source+" IN ACCESS SHARE MODE")
	require.NoError(t, err)

	// The application's request waits behind the reader for as long as the
	// test lets it.
	blockerCtx, giveUp := context.WithCancel(t.Context())
	t.Cleanup(giveUp)
	blocked := make(chan error, 1)
	wg.Go(func() {
		blocker, err := f.pool.Begin(blockerCtx)
		if err != nil {
			blocked <- err
			return
		}
		defer func() { _ = blocker.Rollback(context.WithoutCancel(blockerCtx)) }()
		if _, err := blocker.Exec(blockerCtx, "SET LOCAL lock_timeout = 0"); err != nil {
			blocked <- err
			return
		}
		_, err = blocker.Exec(blockerCtx, "LOCK TABLE "+source+" IN ACCESS EXCLUSIVE MODE")
		blocked <- err
	})
	const queuedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.waitsForLock(t, shadow.SourceOID(), "AccessExclusiveLock")
	}, queuedDeadline, 20*time.Millisecond, "the application's ACCESS EXCLUSIVE should queue behind the reader")

	polls := make(chan error, 1)
	wg.Go(func() {
		_, err := tracker.Progress(t.Context())
		polls <- err
	})
	const pollDeadline = 15 * time.Second
	select {
	case err := <-polls:
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the poll ends with the server's lock timeout, not the application's patience")
		assert.Equal(t, "55P03", pgErr.Code)
	case <-time.After(pollDeadline):
		t.Fatalf("the poll did not end within %s: it waited behind the application's lock", pollDeadline)
	}

	giveUp()
	require.Error(t, <-blocked, "the application's request was cancelled, never granted")
	require.NoError(t, reader.Rollback(context.WithoutCancel(t.Context())))
	close(resume)
	const finishDeadline = 15 * time.Second
	select {
	case err := <-results:
		require.NoError(t, err)
	case <-time.After(finishDeadline):
		t.Fatalf("copy did not finish within %s of the worker resuming", finishDeadline)
	}
	f.assertConverged(t, shadow)
}

// A row count the copier cannot read ends Run before the clear or any chunk
// runs: the copy is fail-closed on its first statement, the shadow is
// untouched, and the tracker is not left polling a copier that never
// started. The pool here is closed, the plainest way a connection fails.
func TestCopierRefusesToStartWithoutARowCount(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 300)
	closed, err := pgxpool.New(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	closed.Close()
	tracker := copyTracker(t)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 2, Tracker: tracker})
	require.NoError(t, err)
	err = c.Run(t.Context(), closed)
	require.Error(t, err)
	assert.NotErrorIs(t, err, copier.ErrInvariantViolation, "a connection failure is the connection's error, not a refusal")

	assert.Equal(t, copier.Position{}, c.Position(), "nothing was cut or landed")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "no row moved")
	after, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "the tracker no longer asks a copier that did not start")
	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{}, work)
}

// rows_total is the catalog's last-known count, not a scan of the source:
// a table ANALYZE has never visited has none, and the copier reports 0
// rather than the catalog's -1 read as an unsigned number. Autovacuum is
// off for the table so no background ANALYZE can supply a count mid-test.
func TestCopierReportsNoRowsTotalWithoutACatalogCount(t *testing.T) {
	f := newCopierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL,
			note text
		) WITH (autovacuum_enabled = false)`)
	f.exec(t, `
		INSERT INTO %s.orders (id, qty, note)
		SELECT n, n, 'order ' || n FROM generate_series(1, 300) AS n`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 2})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{RowsCopied: 300, RowsTotal: 0}, work)
}

// A resumed copy counts the rows it inserted, not the rows the earlier run
// landed below the watermark, while rows_total still describes the whole
// source: the two are not a fraction of this run.
func TestCopierResumeReportsOnlyThisRunsRows(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 1000
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, "ANALYZE %s.orders")
	// The earlier run landed keys 1..500 and checkpointed watermark 500.
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= 500`, pgx.Identifier{shadow.ShadowTable()}.Sanitize()))

	c, err := copier.NewCopier(target, shadow, lock, copier.NewWatermark(500), copier.Options{Workers: 2})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	f.assertConverged(t, shadow)

	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{RowsCopied: 500, RowsTotal: rows}, work)
}
