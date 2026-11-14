package loadproof_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// watchPollInterval paces the checkpoint poll. It only needs to see each
// phase at least once; the phases of a run over a large table last far
// longer than a tick.
const watchPollInterval = 250 * time.Millisecond

// watched is what the poll saw of the run.
type watched struct {
	// FirstSeen is when the poll first saw the row in each phase.
	FirstSeen map[checkpoint.Phase]time.Time
	// SlotName is the slot the row named, for forensics on a leftover.
	SlotName string
	// PeakSlotLagBytes is the most WAL the poll ever saw written past the
	// slot's confirmed flush position; PeakSlotLagByPhase is the same per
	// phase the poll saw it in, which tells a lag that grew in one phase
	// from one that was there throughout.
	PeakSlotLagBytes   int64
	PeakSlotLagByPhase map[checkpoint.Phase]int64
	// Mark is the load's progress at the last poll that saw the row say
	// the swap had not happened, MarkPhase is the phase that poll saw, and
	// MarkAt is when it read the progress; MarkSet is whether any poll did.
	Mark      testutil.Progress
	MarkPhase checkpoint.Phase
	MarkAt    time.Time
	MarkSet   bool
	// Err is the first error the poll met that it could not skip.
	Err error
}

// watch is a running checkpoint poll.
type watch struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	seen   watched
}

// startWatch polls the run's checkpoint row and the slot it names while
// load writes the table, through the checkpoint store's inspection — the
// same read a status command makes, so the harness needs no hook into the
// binary. At every poll it reads the load's progress before it reads the
// row: a mutation counts once its commit returns, so when the row then says
// the swap has not happened, every counted mutation is in the source the
// swap will retain. The last such progress is the mark: the row oracle
// excludes the keys touched after its Touched count, and a load failure
// before its time cannot have been caused by the swap.
func startWatch(t *testing.T, pool *pgxpool.Pool, table testutil.WorkloadTable, load *testutil.LoadGenerator) *watch {
	t.Helper()
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	w := &watch{cancel: cancel, seen: watched{
		FirstSeen:          map[checkpoint.Phase]time.Time{},
		PeakSlotLagByPhase: map[checkpoint.Phase]int64{},
	}}
	w.wg.Go(func() {
		ticker := time.NewTicker(watchPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			progress, readAt := load.Progress(), time.Now()
			cp, err := store.Inspect(ctx, table.Schema, table.Table)
			if errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing) || errors.Is(err, context.Canceled) {
				continue
			}
			if err != nil {
				w.fail(err)
				return
			}
			lag, err := slotLag(ctx, pool, cp.SlotName)
			if err != nil && !errors.Is(err, context.Canceled) {
				w.fail(err)
				return
			}
			w.record(cp, progress, readAt, lag)
		}
	})
	// A test that ends before it stops the poll — interrupted, or failed
	// while the binary ran — still waits for it before the leftover
	// cleanup and the pool close run.
	t.Cleanup(func() { w.stop() })
	return w
}

// beforeSwap reports whether a row in phase was saved before the swap
// committed: the cutover save precedes the swap, so a row that has not
// reached it describes a source every committed write is still in.
func beforeSwap(phase checkpoint.Phase) bool {
	switch phase {
	case checkpoint.PhaseCopying, checkpoint.PhaseCatchingUp, checkpoint.PhaseReconciling, checkpoint.PhaseVerifying:
		return true
	default:
		return false
	}
}

func (w *watch) record(cp checkpoint.Checkpoint, progress testutil.Progress, readAt time.Time, lag int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen.FirstSeen[cp.Phase]; !ok {
		w.seen.FirstSeen[cp.Phase] = time.Now()
	}
	w.seen.SlotName = cp.SlotName
	w.seen.PeakSlotLagBytes = max(w.seen.PeakSlotLagBytes, lag)
	w.seen.PeakSlotLagByPhase[cp.Phase] = max(w.seen.PeakSlotLagByPhase[cp.Phase], lag)
	if beforeSwap(cp.Phase) {
		w.seen.Mark, w.seen.MarkPhase, w.seen.MarkAt, w.seen.MarkSet = progress, cp.Phase, readAt, true
	}
}

func (w *watch) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen.Err == nil {
		w.seen.Err = err
	}
}

// stop ends the poll and returns what it saw.
func (w *watch) stop() watched {
	w.cancel()
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen
}

// slotLag returns how far the server's WAL write position is past slot's
// confirmed flush position, or zero when the row names no slot or the
// slot is already gone.
func slotLag(ctx context.Context, pool *pgxpool.Pool, slot string) (int64, error) {
	if slot == "" {
		return 0, nil
	}
	var lag int64
	err := pool.QueryRow(ctx, `
		SELECT (pg_current_wal_lsn() - coalesce(confirmed_flush_lsn, restart_lsn))::bigint
		FROM pg_replication_slots
		WHERE slot_name = $1`, slot).Scan(&lag)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return lag, nil
}
