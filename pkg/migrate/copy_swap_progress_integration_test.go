package migrate

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// heldTx is a transaction a checkpoint hook opened to hold the run at a
// known point, on a connection of its own so the hold takes nothing from
// the run's pool; the test rolls it back once it has observed the run
// there.
type heldTx struct {
	opened chan struct{}
	err    error
	conn   *pgx.Conn
	once   sync.Once
}

func newHeldTx() *heldTx { return &heldTx{opened: make(chan struct{})} }

// open runs sql in a transaction on a new connection to f's database and
// leaves the transaction open; a failure is kept for wait to report on the
// test's goroutine, since the hook runs on the run's.
func (h *heldTx) open(ctx context.Context, f copySwapFixture, sql string) {
	defer close(h.opened)
	conn, err := pgx.Connect(ctx, f.cfg.URL)
	if err != nil {
		h.err = fmt.Errorf("connect the hold: %w", err)
		return
	}
	h.conn = conn
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		h.err = fmt.Errorf("begin the hold: %w", err)
		return
	}
	if _, err := conn.Exec(ctx, sql); err != nil {
		h.err = fmt.Errorf("hold with %q: %w", sql, err)
	}
}

// wait blocks until the hold is open and fails the test if opening it
// failed.
func (h *heldTx) wait(t *testing.T) {
	t.Helper()
	const holdDeadline = 30 * time.Second
	select {
	case <-h.opened:
	case <-time.After(holdDeadline):
		t.Fatalf("the run did not reach the hold within %s", holdDeadline)
	}
	require.NoError(t, h.err)
}

// release ends the hold, once: closing the connection rolls its
// transaction back.
func (h *heldTx) release(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		if h.conn != nil {
			assert.NoError(t, h.conn.Close(context.WithoutCancel(t.Context())))
		}
	})
}

// awaitWork polls tracker until a snapshot of operation carries work that
// done accepts, and returns it.
func awaitWork(t *testing.T, tracker *progress.Tracker, operation progress.Operation, done func(progress.Work) bool) progress.Snapshot {
	t.Helper()
	const workDeadline = 30 * time.Second
	var observed progress.Snapshot
	require.Eventually(t, func() bool {
		snapshot, err := tracker.Progress(t.Context())
		if err != nil || snapshot.Detail.Operation != operation || snapshot.Detail.Work == nil {
			return false
		}
		observed = snapshot
		return done(*snapshot.Detail.Work)
	}, workDeadline, 10*time.Millisecond, "the run must report %s work", operation)
	return observed
}

// A tracker handed to Run follows a copy-and-swap run through its phases,
// each reported as the statement's step with its own counters. Two holds
// make each poll land on one known state. The copy is held by a row the
// test inserts, uncommitted, into the shadow as the run enters copying:
// the copier has registered its counters and read the analyzed source's
// 2500 rows as its total, and its first act — fencing the shadow before it
// clears it — waits for that transaction, so it has copied nothing yet.
// The verification pass is held by an ACCESS EXCLUSIVE lock the test takes
// on the shadow as the run enters verifying, so the pass has registered
// its counters and compared nothing yet. The run is quiesced, so it has
// no catch-up phase.
func TestRunReportsACopyAndSwapRunsPhasesToTheTracker(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.exec(t, `ANALYZE {schema}.orders`)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	holdCopy, holdPass := newHeldTx(), newHeldTx()
	var copyOnce, holdOnce sync.Once
	opts := f.options()
	opts.Progress = tracker
	opts.CopySwap.Copier = copier.Options{
		LockTimeout: 30 * time.Second, // the copy waits on the hold, not on a short default
	}
	opts.CopySwap.OnCheckpoint = func(_ context.Context, cp checkpoint.Checkpoint) error {
		shadow := pgx.Identifier{cp.Schema, cp.ShadowTable}.Sanitize()
		switch cp.Phase {
		case checkpoint.PhaseCopying:
			copyOnce.Do(func() {
				holdCopy.open(t.Context(), f, `INSERT INTO `+shadow+` (id, qty) VALUES (1250, 0)`)
			})
		case checkpoint.PhaseVerifying:
			holdOnce.Do(func() {
				holdPass.open(t.Context(), f, `LOCK TABLE `+shadow+` IN ACCESS EXCLUSIVE MODE`)
			})
		}
		return nil
	}
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // registered first, so it runs after the holds are released
	t.Cleanup(func() {
		holdCopy.release(t)
		holdPass.release(t)
	})
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	type result struct {
		v   verdict.Verdict
		err error
	}
	done := make(chan result, 1)
	wg.Go(func() {
		v, runErr := Run(t.Context(), f.pool, st, opts)
		done <- result{v: v, err: runErr}
	})

	holdCopy.wait(t)
	copying := awaitWork(t, tracker, progress.OperationCopy, func(w progress.Work) bool { return w.RowsTotal == 2500 })
	holdCopy.release(t)
	assert.Equal(t, progress.PhaseRunning, copying.Phase)
	assert.Equal(t, 1, copying.Step)
	assert.Equal(t, 1, copying.TotalSteps)
	assert.Zero(t, copying.Detail.Work.RowsCopied, "the held copy has landed no chunk")
	assert.Positive(t, copying.Detail.Work.BytesTotal, "the copier measures the source's size")
	assert.Zero(t, copying.Detail.Work.ChunksCompared, "the copy reports no checksum counters")

	holdPass.wait(t)
	verifying := awaitWork(t, tracker, progress.OperationChecksum, func(progress.Work) bool { return true })
	holdPass.release(t)
	assert.Equal(t, 1, verifying.Step)
	assert.Equal(t, progress.Work{}, *verifying.Detail.Work,
		"the held pass has compared nothing yet, and reports none of the copy's counters")

	r := <-done
	require.NoError(t, r.err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, r.v.Outcome)
	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, end.Phase)
	assert.Equal(t, 1, end.Step)
	assert.Equal(t, progress.OperationChecksum, end.Detail.Operation, "the verification pass was the last phase reported")
	assert.Nil(t, end.Detail.Work, "a terminal snapshot carries no work")
}

// The run's tracker and the copier's own are two ways to observe one copy,
// and a tracker reads one work source at a time, so a policy that sets
// both is refused before anything runs.
func TestRunRefusesTheRunTrackerBesideTheCopierTracker(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	runTracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	copyTracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := f.options()
	opts.Progress = runTracker
	opts.CopySwap.Copier.Tracker = copyTracker
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	v, err := Run(t.Context(), f.pool, st, opts)
	require.ErrorContains(t, err, "leave Options.CopySwap.Copier.Tracker nil")
	assert.Equal(t, verdict.Verdict{}, v)
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "nothing was executed")
}
