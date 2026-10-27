package applier_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// A commit the server has flushed to the WAL but not yet made visible — one
// waiting for a synchronous standby — is delivered by the stream all the
// same. The catch-up buffers the change and does not apply it while the
// source's snapshot still hides its transaction, since a read of the source
// taken then would miss it (CO-4); once the wait ends and the transaction is
// visible, the change is applied and the shadow converges.
//
// The server names a standby that never connects, so a session that asks
// for synchronous commit waits in its commit until the test cancels the
// wait; every other session commits locally and never waits.
func TestCatchupWaitsForACommitTheServerHasNotMadeVisible(t *testing.T) {
	f := newCatchupFixtureWithSettings(t, 20,
		"synchronous_commit=local",
		"synchronous_standby_names=blocked")
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})

	// The writer is its own connection, outside the pool's statement
	// timeout: its commit is meant to wait.
	writer, err := pgx.Connect(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, writer.Close(context.WithoutCancel(t.Context())))
	})
	var writerPID int
	require.NoError(t, writer.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&writerPID))
	_, err = writer.Exec(t.Context(), `SET synchronous_commit = on`)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var commitErr error
	wg.Go(func() {
		_, commitErr = writer.Exec(t.Context(), `UPDATE `+f.table.Qualified()+` SET amount = 99 WHERE id = 2`)
	})
	t.Cleanup(func() {
		// A writer still waiting when the test fails is released so the
		// connection can close.
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_cancel_backend($1)`, writerPID)
		assert.NoError(t, err)
		wg.Wait()
	})

	run.awaitStatus(t, "buffer the delivered change without applying it", func(s applier.Status) bool {
		return s.Invisible == 1 && s.Buffered == 1
	})
	waiting := run.status(t)
	assert.Zero(t, waiting.Applied, "nothing reaches the shadow while the source hides the transaction")
	assert.Less(t, waiting.Confirmed, waiting.Delivered, "the slot is not confirmed past the waiting change")

	// Cancelling a backend that is waiting for a synchronous standby ends
	// the wait with a warning; the transaction has committed locally and
	// becomes visible.
	var cancelled bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_cancel_backend($1)`, writerPID).Scan(&cancelled))
	require.True(t, cancelled)
	wg.Wait()
	require.NoError(t, commitErr, "the cancelled wait is a warning, not a failed commit")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "apply the change once its transaction is visible", func(s applier.Status) bool {
		return s.Applied >= 1 && s.Buffered == 0 && s.Confirmed >= end
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}
