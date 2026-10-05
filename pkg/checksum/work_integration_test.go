package checksum_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
)

// checksumTracker is a tracker inside a running checksum step, the state in
// which an orchestrator polls a pass.
func checksumTracker(t *testing.T) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationChecksum)
	tracker.StartStep(1, progress.OperationChecksum, "")
	return tracker
}

// workObserver polls the tracker after every statement a pass runs and
// keeps each poll's counters in order, so a test can ask what an observer
// saw at each point of the pass rather than only at its end. The hook runs
// inside pgx's tracer on the pass's goroutine with a connection checked
// out, so it records and never asserts: a failed assertion there would
// leave the pool's Close waiting for that connection forever. The test
// asserts on the record once the pass has returned.
type workObserver struct {
	tracker *progress.Tracker
	mu      sync.Mutex
	seen    []progress.Work
	// failed is the first poll that erred or carried no engine work.
	failed error
}

var errPollWithoutWork = errors.New("a poll during the pass carried no engine work")

// pool is a pool whose every statement is followed by one poll of the
// tracker, on the pass's own goroutine.
func (o *workObserver) pool(t *testing.T, f verifierFixture) *pgxpool.Pool {
	t.Helper()
	return f.hookedPool(t, func(ctx context.Context, _ *pgx.Conn, _ string) {
		snapshot, err := o.tracker.Progress(ctx)
		o.mu.Lock()
		defer o.mu.Unlock()
		switch {
		case err != nil:
			o.fail(err)
		case snapshot.Detail.Work == nil:
			o.fail(errPollWithoutWork)
		default:
			o.seen = append(o.seen, *snapshot.Detail.Work)
		}
	})
}

// fail keeps the first failure; the caller holds o.mu.
func (o *workObserver) fail(err error) {
	if o.failed == nil {
		o.failed = err
	}
}

// requireEveryPollCarriedWork fails the test if any poll during the pass
// erred or found no work source registered.
func (o *workObserver) requireEveryPollCarriedWork(t *testing.T) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	require.NoError(t, o.failed, "every statement of a pass runs while the verifier is the work source")
}

// snapshots returns every poll so far, oldest first.
func (o *workObserver) snapshots() []progress.Work {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]progress.Work(nil), o.seen...)
}

// forget drops the polls recorded so far, so the next pass is observed on
// its own.
func (o *workObserver) forget() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen, o.failed = nil, nil
}

// first returns the earliest poll that satisfies want, and whether one did.
func (o *workObserver) first(want func(progress.Work) bool) (progress.Work, bool) {
	for _, w := range o.snapshots() {
		if want(w) {
			return w, true
		}
	}
	return progress.Work{}, false
}

// A repair pass over a shadow that differs in every chunk is visible to a
// poller stage by stage: each chunk's digest counts the source's rows the
// moment it commits, the three mismatches are counted before the repair
// transaction opens, the repair moves chunks_repaired to three at its one
// commit, and the rereads then count one by one as chunks_reread while
// chunks_compared stands still. Once Check returns the tracker no longer
// asks the verifier, whose own counters still hold the whole pass.
func TestCheckReportsEachStageOfARepairPass(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)
	observer := &workObserver{tracker: checksumTracker(t)}
	pool := observer.pool(t, f)

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Tracker: observer.tracker})
	require.NoError(t, err)
	outcome, err := v.Check(t.Context(), pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 3)
	observer.requireEveryPollCarriedWork(t)

	firstChunk, ok := observer.first(func(w progress.Work) bool { return w.ChunksCompared == 1 })
	require.True(t, ok, "a poll lands between the first chunk's commit and the second's first statement")
	assert.Equal(t, uint64(1000), firstChunk.RowsHashed, "the first chunk's digest covers the source's 1000 keys, not the shadow's 999")
	assert.Equal(t, uint64(1), firstChunk.ChunksMismatched, "the first chunk's missing row is counted with its digest")
	assert.Zero(t, firstChunk.ChunksRepaired)

	compared, ok := observer.first(func(w progress.Work) bool { return w.ChunksCompared == 3 })
	require.True(t, ok, "the repair transaction's first statement is polled with the comparison complete")
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3}, compared, "every chunk differs and none has been repaired yet")

	repaired, ok := observer.first(func(w progress.Work) bool { return w.ChunksRepaired > 0 })
	require.True(t, ok, "the rereads are polled after the repair committed")
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3, ChunksRepaired: 3}, repaired, "one transaction repairs all three chunks, so the count moves from zero to three at once, before any reread")

	reread, ok := observer.first(func(w progress.Work) bool { return w.ChunksReread == 2 })
	require.True(t, ok, "the third reread's first statement is polled with two rereads committed")
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3, ChunksRepaired: 3, ChunksReread: 2}, reread, "a reread counts as a reread, not as a compared chunk or as hashed rows")

	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a returned pass is no longer the tracker's work source")
	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3, ChunksRepaired: 3, ChunksReread: 3}, direct, "three chunks compared, then the same three repaired and reread")
}

// A clean Verify pass counts its three chunks and the source's rows and
// nothing else, and a second pass on the same verifier starts from zero:
// the first poll of the second pass sees no chunk compared, so a poller
// never reads the previous pass's total as this pass's progress. The
// pass's final figure is read from the verifier after it returns: the last
// statement of a pass is the last chunk's commit, polled before that chunk
// is counted.
func TestVerifyCountsAPassAndTheNextPassStartsFromZero(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	observer := &workObserver{tracker: checksumTracker(t)}
	pool := observer.pool(t, f)

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Tracker: observer.tracker})
	require.NoError(t, err)
	report, err := v.Verify(t.Context(), pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.True(t, report.Clean())
	observer.requireEveryPollCarriedWork(t)

	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500}, direct)
	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a returned pass is no longer the tracker's work source")

	observer.forget()
	_, err = v.Verify(t.Context(), pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	observer.requireEveryPollCarriedWork(t)
	seen := observer.snapshots()
	require.NotEmpty(t, seen)
	assert.Equal(t, progress.Work{}, seen[0], "the second pass's first statement is polled with every counter reset")
	direct, err = v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500}, direct, "the second pass ends with only its own chunks counted")
}

// Under DivergenceAbort the pass counts what it found and repairs nothing:
// chunks_mismatched is the only divergence signal an observer gets, so it
// must move even when the policy refuses the repair.
func TestCheckUnderAbortCountsMismatchesAndNoRepairs(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)
	observer := &workObserver{tracker: checksumTracker(t)}
	pool := observer.pool(t, f)

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Tracker: observer.tracker})
	require.NoError(t, err)
	_, err = v.Check(t.Context(), pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort)
	var divergence *checksum.DivergenceError
	require.ErrorAs(t, err, &divergence)
	observer.requireEveryPollCarriedWork(t)

	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3}, direct, "the comparison ran to the end; the policy stopped the pass before any repair")
	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a refused pass has released the tracker too")
}

// A repair whose transaction rolls back repaired nothing, so the counters
// show the mismatch the comparison found and no repair: chunks_repaired
// moves only when the recopy commits, never when it starts. The lock is
// lost as the repair's first statement begins, which cancels the repair's
// transaction.
func TestCheckCountsNoRepairWhenTheRepairRollsBack(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, buildLock)
	require.NoError(t, buildLock.Release(t.Context()))
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	pool := f.poolHookedBeforeStatement(t, "DELETE FROM", func() {
		f.terminateBackend(t, lock.BackendPID())
		const lockLossDeadline = 15 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
	})

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Tracker: checksumTracker(t)})
	require.NoError(t, err)
	_, err = v.Check(t.Context(), pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 1}, work, "the rolled-back recopy repaired nothing and reread nothing")
}

// An observer polls from its own goroutine, as an orchestrator does, for
// the whole of a repair pass: the pass finishes, every poll succeeds, and
// the counters are read under the race detector. A poll inside Work while
// the pass counts, or while its stop fences the tracker, must neither race
// nor deadlock.
func TestCheckFinishesUnderAConcurrentPoller(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)
	tracker := checksumTracker(t)
	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Tracker: tracker})
	require.NoError(t, err)

	done := make(chan struct{})
	pollErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				pollErr <- nil
				return
			default:
			}
			if _, err := tracker.Progress(t.Context()); err != nil {
				pollErr <- err
				return
			}
		}
	})
	result := make(chan error, 1)
	wg.Go(func() {
		_, err := v.Check(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
		result <- err
	})
	const passDeadline = 20 * time.Second
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(passDeadline):
		t.Fatalf("Check did not return within %s under a concurrent poller", passDeadline)
	}
	close(done)
	wg.Wait()
	require.NoError(t, <-pollErr, "every poll during the pass succeeded")
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3, ChunksRepaired: 3, ChunksReread: 3}, work)
}
