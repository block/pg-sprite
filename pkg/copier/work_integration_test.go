package copier_test

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
