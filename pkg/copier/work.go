package copier

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/progress"
)

// rowsTotalSQL reads the catalog's last-known row count of the source: the
// figure ANALYZE or VACUUM recorded, -1 for a table neither has visited. A
// relation that no longer exists also reads -1: the count is a measurement,
// and whether the source is still the proven relation is the chunk guard's
// question (ST-6), answered from inside the write transaction.
const rowsTotalSQL = `SELECT COALESCE((SELECT reltuples FROM pg_class WHERE oid = $1::oid), -1)`

// tableSizesSQL measures the shadow and the source on disk at one instant —
// heap, TOAST, and maps, without indexes. A relation that no longer exists
// measures NULL, which reads as unmeasured.
const tableSizesSQL = `SELECT COALESCE(pg_table_size($1::oid), 0), COALESCE(pg_table_size($2::oid), 0)`

// Work reports the copy's counters for the progress tracker: it is the
// progress.WorkSource the copier registers for the lifetime of Run.
//
//   - rows_copied is the number of rows this run's committed chunks inserted
//     into the shadow, exact and monotone; a resumed run counts only its own
//     rows, never those the earlier run landed below the watermark.
//   - rows_total is the catalog's last-known row count of the source at the
//     start of Run (pg_class.reltuples), 0 for a table ANALYZE has never
//     visited.
//   - bytes_copied and bytes_total are the shadow's and the source's on-disk
//     table sizes measured at the poll. The two tables differ in shape, so
//     the shadow finishes smaller or larger than the source; the pair is a
//     measured size, not a fraction.
//
// Outside Run the copier holds no connection, so the sizes read as 0 while
// the row counters still report what the ledger knows.
func (c *Copier) Work(ctx context.Context) (progress.Work, error) {
	c.mu.Lock()
	db, rowsTotal, rows := c.db, c.rowsTotal, c.ledger.rows
	c.mu.Unlock()
	work := progress.Work{RowsCopied: uint64(rows), RowsTotal: rowsTotal}
	if db == nil {
		return work, nil
	}
	var shadowBytes, sourceBytes int64
	if err := db.QueryRow(ctx, tableSizesSQL, c.shadow.ShadowOID(), c.shadow.SourceOID()).Scan(&shadowBytes, &sourceBytes); err != nil {
		return work, fmt.Errorf("measure copy of %s.%s into %s: %w", c.target.Schema(), c.target.Table(), c.shadow.ShadowTable(), err)
	}
	work.BytesCopied, work.BytesTotal = uint64(shadowBytes), uint64(sourceBytes)
	return work, nil
}

// measureRowsTotal records the source's catalog row count for the run. A
// negative figure means the catalog has none, which reports as 0 rather
// than as an unsigned wraparound.
func (c *Copier) measureRowsTotal(ctx context.Context, pool *pgxpool.Pool) error {
	var relTuples float64
	if err := pool.QueryRow(ctx, rowsTotalSQL, c.shadow.SourceOID()).Scan(&relTuples); err != nil {
		return fmt.Errorf("read the row count of %s.%s: %w", c.target.Schema(), c.target.Table(), err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rowsTotal = 0
	if relTuples > 0 {
		c.rowsTotal = uint64(relTuples)
	}
	return nil
}

// report binds pool as the connection Work measures over and registers the
// copier with the tracker, when there is one, for the rest of Run. The
// returned stop is the fence before Run returns: it waits for an in-flight
// poll and then unbinds the pool, so no poll reaches the pool after Run
// (the pool is the caller's, and a finished copier opens nothing on it).
func (c *Copier) report(pool *pgxpool.Pool) (stop func()) {
	c.mu.Lock()
	c.db = pool
	c.mu.Unlock()
	if c.opts.Tracker != nil {
		c.opts.Tracker.SetWorkSource(c)
	}
	return func() {
		if c.opts.Tracker != nil {
			c.opts.Tracker.StopWorkSource()
		}
		c.mu.Lock()
		c.db = nil
		c.mu.Unlock()
	}
}
