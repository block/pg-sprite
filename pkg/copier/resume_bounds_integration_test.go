package copier_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// A clear that took the whole tail in one statement would hold its locks
// and its snapshot for as long as the tail is long; ClearAbove instead
// removes the tail in batches the caller's chunker sizes, each in its own
// guarded transaction, and stops at the first batch that comes up short.
// With the shadow holding every key and the resume watermark at 100, a
// batch of 50 takes the 200 keys above in four full batches and learns
// from an empty fifth that nothing is left; the keys through the watermark
// are untouched. The batch count is read from the statements the pool
// sent, not inferred from timing.
func TestClearAboveRemovesTheTailInChunkerSizedBatches(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepareClearedShadow(t, 300)
	sent := newStatementLog()
	pool := f.tracedPool(t, sent)

	err := copier.ClearAbove(t.Context(), pool, target, shadow, lock, copier.NewWatermark(100), copier.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	require.NoError(t, err)

	assert.Equal(t, 5, sent.count(clearStatementPrefix(f, shadow)), "four full batches of 50, then the short batch that ends the clear")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id > 100"), "every key above the watermark is gone")
	assert.Equal(t, int64(100), f.count(t, shadow.ShadowTable(), "id <= 100"), "every key through the watermark stays")
}

// A tail that is not a multiple of the batch ends on the short batch, not
// on an extra empty one, and only the first batch is the fence. With the
// shadow holding keys 1..275 and the watermark at 100, a batch of 50 takes
// the 175 keys above in three full batches and a short one of 25, which
// ends the clear: four deletes. The first batch's guard takes the shadow
// in SHARE ROW EXCLUSIVE to wait out the earlier run's stragglers; the
// three after it hold ROW EXCLUSIVE, what their delete holds, so none
// queues behind a writer the fence already waited out.
func TestClearAboveEndsAtTheFirstShortBatchAndFencesOnlyTheFirst(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepareClearedShadow(t, 275)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	sent := newStatementLog()
	pool := f.tracedPool(t, sent)

	err := copier.ClearAbove(t.Context(), pool, target, shadow, lock, copier.NewWatermark(100), copier.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	require.NoError(t, err)

	assert.Equal(t, 4, sent.count(clearStatementPrefix(f, shadow)), "three full batches of 50, then the short batch of 25 that ends the clear")
	assert.Equal(t, 1, sent.count("LOCK TABLE "+shadowName+" IN SHARE ROW EXCLUSIVE MODE"), "only the first batch is the fence")
	assert.Equal(t, 3, sent.count("LOCK TABLE "+shadowName+" IN ROW EXCLUSIVE MODE"), "every later batch holds what its delete holds")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id > 100"), "every key above the watermark is gone")
	assert.Equal(t, int64(100), f.count(t, shadow.ShadowTable(), "id <= 100"), "every key through the watermark stays")
}

// Every clear batch runs under the caller's lock_timeout and
// statement_timeout, not the pool's session defaults and not the copier's
// own (LK-2): the fence waits behind a straggling writer for exactly as
// long as the caller allowed. Here a second connection holds the shadow in
// ROW EXCLUSIVE, as a straggling chunk transaction does, for the whole
// test; the fence runs out of the one-second budget the caller set, ends
// with the server's lock_not_available, and removes nothing. The budget
// the batch set is read from the statement the pool sent, so a clear that
// fell back to a default would be caught however long the default is.
func TestClearAboveEndsAtTheCallersLockTimeoutBehindAHeldShadowLock(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepareClearedShadow(t, 300)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	sent := newStatementLog()
	pool := f.tracedPool(t, sent)

	straggler, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	// Redundant safety closer: the test rolls the straggler back on its
	// last line, after which Rollback returns the guaranteed ErrTxClosed.
	t.Cleanup(func() { _ = straggler.Rollback(context.WithoutCancel(t.Context())) })
	_, err = straggler.Exec(t.Context(), "LOCK TABLE "+shadowName+" IN ROW EXCLUSIVE MODE")
	require.NoError(t, err)

	err = copier.ClearAbove(t.Context(), pool, target, shadow, lock, copier.NewWatermark(100), copier.Options{
		LockTimeout:      time.Second,
		StatementTimeout: 5 * time.Second,
		Chunker:          copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
	})
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the clear ends with the server's lock timeout, not the caller's patience")
	assert.Equal(t, "55P03", pgErr.Code)

	assert.Equal(t, 1, sent.count("SET LOCAL lock_timeout = 1000; SET LOCAL statement_timeout = 5000"), "the one batch that ran set the caller's budgets")
	assert.Equal(t, 0, sent.count(clearStatementPrefix(f, shadow)), "the fence was never granted, so no delete was sent")
	assert.Equal(t, int64(300), f.count(t, shadow.ShadowTable(), "TRUE"), "nothing was removed")
	require.NoError(t, straggler.Rollback(t.Context()))
}

// A clear whose lock session dies mid-batch must not finish the batch: the
// lock is what keeps every other writer off the table, and a delete that
// commits after it is gone acts on a shadow the engine no longer owns
// (LK-1). ClearAbove runs each batch under the session's Bind context, so
// the loss cancels the statement in flight and the batch's transaction is
// rolled back with it. Here the first batch's delete is held just before it
// is sent, the session's backend is terminated, and the session's keepalive
// reports the loss; the clear then returns the cancellation without the
// delete ever running, and the shadow still holds every key it started
// with.
func TestClearAboveAbandonsTheBatchInFlightWhenTheLockIsLost(t *testing.T) {
	f := newCopierFixture(t)
	f.createOrders(t, 300)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target)
	require.NoError(t, buildLock.Release(t.Context()))
	f.seedShadow(t, shadow, 300)
	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))

	parked := newClearDeletePause()
	pool := f.pausedPool(t, parked)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		results <- copier.ClearAbove(t.Context(), pool, target, shadow, lock, copier.NewWatermark(100), copier.Options{
			LockTimeout: 30 * time.Second,
			Chunker:     copier.ChunkerOptions{InitialRows: 50, MaxRows: 50},
		})
	})
	t.Cleanup(wg.Wait)
	t.Cleanup(parked.resume)

	const parkDeadline = 15 * time.Second
	select {
	case <-parked.parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the clear did not reach its first batch's delete within %s", parkDeadline)
	}
	f.terminateBackend(t, lock.BackendPID())

	// The loss alone lets the clear go: the parked delete is released by
	// its cancelled context, not by the test.
	const lockLossDeadline = 15 * time.Second
	err := awaitRun(t, results, lockLossDeadline, "the clear")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "the batch in flight was cancelled")
	assert.Error(t, lock.Err(), "the session reported the loss that cancelled it")
	assert.Equal(t, int64(300), f.count(t, shadow.ShadowTable(), "TRUE"), "the cancelled batch did not commit")
}

// prepareClearedShadow prepares an orders table of rows rows and a shadow
// holding every one of them, as a shadow does after an earlier run copied
// past the watermark a resume will clear from.
func (f copierFixture) prepareClearedShadow(t *testing.T, rows int64) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	target, lock, shadow := f.prepare(t, rows)
	f.seedShadow(t, shadow, rows)
	return target, lock, shadow
}

// seedShadow copies keys 1..rows of the source into the shadow directly.
func (f copierFixture) seedShadow(t *testing.T, shadow schemachange.BuiltShadow, rows int64) {
	t.Helper()
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.%s (id, qty)
		SELECT id, qty FROM %%s.orders WHERE id <= %d`, pgx.Identifier{shadow.ShadowTable()}.Sanitize(), rows))
}

// clearStatementPrefix is how every clear batch's delete begins on the
// wire; the bounds follow as bind parameters.
func clearStatementPrefix(f copierFixture, shadow schemachange.BuiltShadow) string {
	return "DELETE FROM " + pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize() + " WHERE"
}

// statementLog is a pgx tracer that records every statement its connections
// send, in the order they were sent.
type statementLog struct {
	mu   sync.Mutex
	sent []string
}

func newStatementLog() *statementLog { return &statementLog{} }

func (l *statementLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, data.SQL)
	return ctx
}

func (l *statementLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// count reports how many recorded statements begin with prefix.
func (l *statementLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, sql := range l.sent {
		if strings.HasPrefix(sql, prefix) {
			n++
		}
	}
	return n
}
