package migrate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// starvedPastSenderTimeout is how long the test keeps the run's catch-up
// from every pool connection: several times the live fixture's
// wal_sender_timeout, so a stream nothing answers for while the catch-up
// waits would be ended by the server within it.
const starvedPastSenderTimeout = 6 * time.Second

// holdSpareConns acquires n connections of the run's pool — every one
// beyond the copy workers' — and returns the release that gives them back,
// so with the workers standing on held chunks no connection is left for
// the catch-up's flushes and reads. The release runs at cleanup too, for a
// test that failed before it gave them back; a second call does nothing.
func holdSpareConns(t *testing.T, pool *pgxpool.Pool, n int) (releaseSpare func()) {
	t.Helper()
	var (
		once  sync.Once
		conns []*pgxpool.Conn
	)
	releaseSpare = func() {
		once.Do(func() {
			for _, c := range conns {
				c.Release()
			}
		})
	}
	t.Cleanup(releaseSpare)
	ctx, cancel := context.WithTimeout(t.Context(), poolFloorDeadline)
	defer cancel()
	for range n {
		c, err := pool.Acquire(ctx)
		require.NoError(t, err, "acquire a spare connection of the run's pool")
		conns = append(conns, c)
	}
	return releaseSpare
}

// A live run whose catch-up waits on the pool for longer than
// wal_sender_timeout keeps its stream and finishes in one Run. The pool is
// at the floor; the test stands the four copy workers on held chunks and
// takes the floor's two spare connections itself, so the catch-up can
// neither flush nor read anything while the server's sender timeout runs
// out several times over, and the source write to key 1 stays out of the
// shadow for the whole window. The stream's heartbeat answers the server
// meanwhile: once the test gives the spare connections back, the write is
// flushed while the workers still wait — the same stream carrying it, not
// a resumed one — and releasing the holds lets the run finish with both
// oracles holding on the result.
func TestRunCopySwapKeepsItsStreamWhileTheCatchupWaitsOnThePool(t *testing.T) {
	shape := testutil.WorkloadShape{IDType: "integer"}
	f := newLiveFixture(t, shape)
	const alter = `ALTER TABLE %s ALTER COLUMN id TYPE bigint`
	copyOpts := poolFloorCopier()
	floor := migrate.CopySwapMinConns(copyOpts)
	pool := f.runPool(t, floor)

	opts := f.options()
	opts.CopySwap.Copier = copyOpts
	opts.CopySwap.CheckpointInterval = 50 * time.Millisecond

	var (
		v verdict.Verdict
		// runErr is set once waitForRun returns.
		runErr error
	)
	waitForRun := startRun(t, func() {
		v, runErr = migrate.Run(context.WithoutCancel(t.Context()), pool, parseOne(t, fmt.Sprintf(alter, f.table.Qualified())), opts)
	})

	f.waitForCopyWriting(t)
	hold := f.holdShadowRows(t, heldKeys(firstHeldChunk, poolFloorChunks))
	f.waitForBlockedWorkers(t, poolFloorWorkers)
	releaseSpare := holdSpareConns(t, pool, int(floor)-poolFloorWorkers)

	f.writeSource(t, 1, flushedLabel)
	pollNever(t, f.shadowHasLabel(1, flushedLabel), starvedPastSenderTimeout,
		"the write to a landed key reached the shadow with every connection of the run's pool held")

	releaseSpare()
	pollUntil(t, f.shadowHasLabel(1, flushedLabel),
		"the write is flushed once the catch-up has a connection again")
	f.assertBlockedWorkers(t, poolFloorWorkers)

	release(t, hold)
	waitForRun()
	require.NoError(t, runErr)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	f.assertZeroDiff(t, shape, alter)
}
