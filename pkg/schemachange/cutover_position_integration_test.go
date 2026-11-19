package schemachange_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// queuedWriterDeadline bounds the wait, inside the drain, for the writer
// started there to queue behind the swap's lock.
const queuedWriterDeadline = 10 * time.Second

// The swap reads the server's WAL position under its ACCESS EXCLUSIVE lock,
// and that position separates the table's writes: an insert that committed
// before the swap lies below it, and an insert that queued behind the
// swap's lock runs once the swap commits, lands in the live table, and
// reads a position above it from inside its own statement. A proof the
// catalog re-derives afterwards cannot know the position and carries zero.
func TestCutoverReportsTheWALPositionThatSeparatesTheSourcesWritesFromTheLiveTables(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	orders := pgx.Identifier{f.schema, "orders"}.Sanitize()
	f.exec(t, `INSERT INTO %s.orders (qty, note, sku) VALUES (1, 'before', 'before')`)
	before := walInsertPosition(t, f)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var queuedID int64
	var queuedAt string
	var queuedErr error
	drain := func(ctx context.Context, tx pgx.Tx) error {
		wg.Go(func() {
			queuedErr = f.pool.QueryRow(context.WithoutCancel(ctx), `
				INSERT INTO `+orders+` (qty, sku)
				VALUES (2, 'queued')
				RETURNING id, pg_current_wal_insert_lsn()::text`).Scan(&queuedID, &queuedAt)
		})
		return awaitQueuedWriter(ctx, tx, s.built.SourceOID())
	}
	swapped, err := schemachange.Cutover(t.Context(), f.pool, s.lock, ready, drain, schemachange.CutoverOptions{})
	wg.Wait()
	require.NoError(t, err)
	require.NoError(t, queuedErr)

	position := swapped.SwapPosition()
	assert.Greater(t, position, before, "an insert committed before the swap lies below the swap's position")
	after, err := decode.ParseLSN(queuedAt)
	require.NoError(t, err)
	assert.Greater(t, after, position, "a writer queued behind the swap's lock runs above the swap's position")
	assert.Equal(t, position, swapped.Proof().SwapPosition)
	var live, old int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+orders+` WHERE id = $1`, queuedID).Scan(&live))
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.schema, swapped.OldTable()}.Sanitize()+` WHERE id = $1`, queuedID).Scan(&old))
	assert.Equal(t, 1, live, "the queued insert landed in the live table")
	assert.Equal(t, 0, old, "and not in the retained source")

	inspected, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	require.NoError(t, err)
	assert.Zero(t, inspected.SwapPosition(), "a re-derived proof cannot know where the WAL stood inside the swap")
}

// A write that lands while the swap waits for its lock, in a transaction
// that began before the swap, commits before the lock is granted, so the
// position the swap reads under the lock lies above the write's. A swap
// that read its position before taking the lock would read below it, and
// the load proof would leave out a write the retained source holds.
func TestSwapPositionIsReadAfterTheLockIsGranted(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	orders := pgx.Identifier{f.schema, "orders"}.Sanitize()
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)

	writer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(context.WithoutCancel(t.Context())) })
	_, err = writer.Exec(t.Context(), `INSERT INTO `+orders+` (qty, sku) VALUES (1, 'first')`)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var swapped schemachange.SwappedTable
	var swapErr error
	wg.Go(func() {
		swapped, swapErr = schemachange.Cutover(t.Context(), f.pool, s.lock, ready, nil, schemachange.CutoverOptions{})
	})
	require.Eventually(t, func() bool {
		var waiting bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_locks
				WHERE NOT granted AND mode = 'AccessExclusiveLock'
			)`).Scan(&waiting))
		return waiting
	}, queuedWriterDeadline, 10*time.Millisecond, "the swap queues for its lock behind the open writer")

	var text string
	require.NoError(t, writer.QueryRow(t.Context(), `
		INSERT INTO `+orders+` (qty, sku)
		VALUES (2, 'second')
		RETURNING pg_current_wal_insert_lsn()::text`).Scan(&text))
	require.NoError(t, writer.Commit(t.Context()))
	wg.Wait()
	require.NoError(t, swapErr)
	written, err := decode.ParseLSN(text)
	require.NoError(t, err)
	assert.Greater(t, swapped.SwapPosition(), written, "a write that committed before the lock was granted lies below the swap's position")
}

// walInsertPosition reads the server's current WAL insert position.
func walInsertPosition(t *testing.T, f shadowFixture) decode.LSN {
	t.Helper()
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&text))
	position, err := decode.ParseLSN(text)
	require.NoError(t, err)
	return position
}

// awaitQueuedWriter polls, inside the swap transaction, until another
// backend waits for a lock on the source, the writer the drain started.
func awaitQueuedWriter(ctx context.Context, tx pgx.Tx, source uint32) error {
	deadline := time.Now().Add(queuedWriterDeadline)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE relation = $1 AND NOT granted AND pid <> pg_backend_pid())`, source).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			return nil
		}
		if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.01)`); err != nil {
			return err
		}
	}
	return fmt.Errorf("the writer did not queue behind the swap lock within %s", queuedWriterDeadline)
}
