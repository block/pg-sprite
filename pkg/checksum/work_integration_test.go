package checksum_test

import (
	"context"
	"math"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
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
// saw at each point of the pass rather than only at its end.
type workObserver struct {
	tracker *progress.Tracker
	mu      sync.Mutex
	seen    []progress.Work
}

// pool is a pool whose every statement is followed by one poll of the
// tracker, on the pass's own goroutine.
func (o *workObserver) pool(t *testing.T, f verifierFixture) *pgxpool.Pool {
	t.Helper()
	return f.hookedPool(t, func(ctx context.Context, _ *pgx.Conn, _ string) {
		snapshot, err := o.tracker.Progress(ctx)
		require.NoError(t, err)
		require.NotNil(t, snapshot.Detail.Work, "every statement of a pass runs while the verifier is the work source")
		o.mu.Lock()
		defer o.mu.Unlock()
		o.seen = append(o.seen, *snapshot.Detail.Work)
	})
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
	o.seen = nil
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
// commit, and the rereads count as compared chunks again. Once Check
// returns the tracker no longer asks the verifier, whose own counters
// still hold the whole pass.
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
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3, ChunksRepaired: 3}, repaired, "one transaction repairs all three chunks, so the count moves from zero to three at once")

	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a returned pass is no longer the tracker's work source")
	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 6, RowsHashed: 5000, ChunksMismatched: 3, ChunksRepaired: 3}, direct, "three chunks compared, then the same three reread after their repair")
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

	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500}, direct)
	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a returned pass is no longer the tracker's work source")

	observer.forget()
	_, err = v.Verify(t.Context(), pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
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

	direct, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{ChunksCompared: 3, RowsHashed: 2500, ChunksMismatched: 3}, direct, "the comparison ran to the end; the policy stopped the pass before any repair")
	after, err := observer.tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Nil(t, after.Detail.Work, "a refused pass has released the tracker too")
}
