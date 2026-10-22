package schemachange_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests below stop a run at a phase transition and resume it with a
// fresh Runner, the way a process that died between two saves is resumed
// (TM-3). The stop is AfterCheckpoint returning an error once the row has
// reached the phase under test, so the row is exactly what a crash right
// after that save would have left.

// errStopHere is the error the fault hook stops a run with.
var errStopHere = errors.New("stop here")

// stopAfter is an AfterCheckpoint hook that stops the run once a save in
// phase has happened, and records every phase it was shown.
type stopAfter struct {
	phase  checkpoint.Phase
	mu     sync.Mutex
	phases []checkpoint.Phase
}

func (s *stopAfter) hook(_ context.Context, cp checkpoint.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phases = append(s.phases, cp.Phase)
	if cp.Phase == s.phase {
		return errStopHere
	}
	return nil
}

func (s *stopAfter) seen() []checkpoint.Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]checkpoint.Phase(nil), s.phases...)
}

// assertResumedToDone runs the table again with a fresh Runner under
// KeepOld, and checks the run reached done with the live table holding the
// source's rows under the statement's shape.
func (f runnerFixture) assertResumedToDone(t *testing.T, alter string, opts schemachange.RunOptions) schemachange.Result {
	t.Helper()
	opts.KeepOld = true
	result, err := f.run(t, "orders", alter, opts)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.False(t, f.hasColumn(t, "orders", "note"))
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: schemachange.OldName(f.schema, "orders")},
		testutil.RelationRef{Schema: f.schema, Table: "orders"},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
	return result
}

// A run stopped right after its first save — the fresh row in copying,
// nothing landed — resumes on the shadow it built rather than building
// another: the second run's shadow is the first's, and the whole copy
// happens in the second run.
func TestRunnerResumesFromTheFreshRow(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCopying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	row := f.inspectRow(t, "orders")
	require.Equal(t, checkpoint.PhaseCopying, row.Phase)
	assert.False(t, row.Watermark.Valid(), "nothing had landed when the run stopped")
	shadowOID := f.relationOID(t, row.ShadowTable)

	result := f.assertResumedToDone(t, alter, schemachange.RunOptions{})
	assert.Equal(t, shadowOID, result.Shadow.ShadowOID, "the resumed run continued on the shadow the first built")
	assert.Equal(t, shadowOID, result.Swapped.LiveOID)
}

// A run stopped after the copy saved verifying resumes without copying
// again: the row's complete watermark sends the second run straight to the
// verification pass, and its saves begin at cutover.
func TestRunnerResumesFromVerifying(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseVerifying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t, "orders").Phase)

	second := &stopAfter{}
	result := f.assertResumedToDone(t, alter, schemachange.RunOptions{AfterCheckpoint: second.hook})
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseCutover, checkpoint.PhaseDone}, second.seen(), "nothing was copied again")
	assert.Equal(t, 3, result.Verification.Chunks, "the resumed run verified the whole key space")
}

// A run stopped after the row said cutover, before the swap ran, finds
// the source still live on resume: the catalog answers not swapped, and
// the second run verifies and gates again before swapping.
func TestRunnerResumesFromCutoverWhenTheSwapNeverRan(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	stop := &stopAfter{phase: checkpoint.PhaseCutover}
	sourceOID := f.relationOID(t, "orders")

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	require.Equal(t, checkpoint.PhaseCutover, f.inspectRow(t, "orders").Phase)
	require.Equal(t, sourceOID, f.relationOID(t, "orders"), "the swap never ran")

	second := &stopAfter{}
	result := f.assertResumedToDone(t, alter, schemachange.RunOptions{AfterCheckpoint: second.hook})
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseDone}, second.seen(), "the row already said cutover")
	assert.Equal(t, 3, result.Verification.Chunks, "the swap was gated on a fresh verification pass")
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
}

// A run that died after its swap committed, before the old table drop,
// left a row in cutover and the shadow under the source's name. The
// resumed run reads that from the catalog (LK-4), makes no verification
// pass — there is no shadow to verify — and continues at the drop.
func TestRunnerResumesFromCutoverAfterTheSwapCommitted(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	sourceOID := f.relationOID(t, "orders")

	// The run up to and including the swap, by hand, with the row saved
	// the way the Runner saves it before calling Cutover.
	lock := f.lock(t, "orders")
	s := f.stageUnder(t, lock, "orders", alter)
	proof, err := json.Marshal(s.built)
	require.NoError(t, err)
	require.NoError(t, f.store.Ensure(t.Context()))
	require.NoError(t, f.store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            f.schema,
		Table:             "orders",
		ShadowTable:       s.built.ShadowTable(),
		Watermark:         copier.NewWatermark(math.MaxInt64),
		SourceFingerprint: s.built.SourceFingerprint(),
		TargetFingerprint: s.built.TargetFingerprint(),
		Phase:             checkpoint.PhaseCutover,
		ShadowProof:       proof,
		Statement:         f.alter(t, alter).SQL(),
	}))
	_, err = f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, lock.Release(t.Context()))

	second := &stopAfter{}
	result, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: second.hook})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseDone}, second.seen())
	assert.Zero(t, result.Verification, "a resume past the swap makes no verification pass")
	assert.True(t, result.SwapProven, "the swap was re-derived from the catalog")
	assert.Equal(t, s.built.ShadowOID(), result.Swapped.LiveOID)
	assert.Equal(t, sourceOID, result.Swapped.OldOID)
	assert.True(t, result.OldTableDropped)
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")))
	assert.False(t, f.hasColumn(t, "orders", "note"))
}

// The same resume when the old table is already gone — the run died
// between the drop's commit and the done save — finds nothing to drop and
// still reaches done.
func TestRunnerResumesFromCutoverAfterTheOldTableWasDropped(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`

	lock := f.lock(t, "orders")
	s := f.stageUnder(t, lock, "orders", alter)
	proof, err := json.Marshal(s.built)
	require.NoError(t, err)
	require.NoError(t, f.store.Ensure(t.Context()))
	require.NoError(t, f.store.Save(t.Context(), lock, checkpoint.Checkpoint{
		Schema:            f.schema,
		Table:             "orders",
		ShadowTable:       s.built.ShadowTable(),
		Watermark:         copier.NewWatermark(math.MaxInt64),
		SourceFingerprint: s.built.SourceFingerprint(),
		TargetFingerprint: s.built.TargetFingerprint(),
		Phase:             checkpoint.PhaseCutover,
		ShadowProof:       proof,
		Statement:         f.alter(t, alter).SQL(),
	}))
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, lock, swapped, schemachange.Options{}))
	require.NoError(t, lock.Release(t.Context()))

	result, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
	assert.True(t, result.OldTableDropped, "the drop had already committed")
	assert.Equal(t, s.built.ShadowOID(), result.Shadow.ShadowOID, "the row's proof names the shadow")
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the shadow bears the source's name")
	assert.False(t, result.SwapProven, "nothing was left to re-derive the swap from")
	assert.Equal(t, schemachange.SwapProof{}, result.Swapped, "the swap's renames cannot be re-derived once the retained source is gone")
}

// hookedClock runs hook on its first reading. The copier reads its clock
// after the first claim and before the first chunk transaction, so a hook
// there meets the copy as a concurrent writer.
type hookedClock struct {
	once sync.Once
	hook func()
}

func (c *hookedClock) Now() time.Time {
	c.once.Do(c.hook)
	return time.Now()
}

// pinShadowKey inserts key into the shadow in a transaction left open at
// the copier's first clock reading, so the chunk holding key waits on the
// primary key and every other chunk lands around it. The returned func
// waits for the pin and rolls it back. The cleanup first disarms the
// hook, so a test that fails before the copier reads its clock ends with
// no pin to roll back instead of waiting on a reading that never comes.
func (f runnerFixture) pinShadowKey(t *testing.T, key int64) (clock *hookedClock, release func()) {
	t.Helper()
	pinned := make(chan struct{})
	shadow := pgx.Identifier{f.schema, schemachange.ShadowName(f.schema, "orders")}.Sanitize()
	var tx pgx.Tx
	var pinErr error
	clock = &hookedClock{hook: func() {
		defer close(pinned)
		tx, pinErr = f.pool.Begin(t.Context())
		if pinErr != nil {
			return
		}
		if _, pinErr = tx.Exec(t.Context(), "INSERT INTO "+shadow+" (id, qty) VALUES ($1, 0)", key); pinErr != nil {
			// A failed insert leaves the transaction holding its pooled
			// connection, and the package's pool.Close waits on it, so it
			// is rolled back here, before release reports pinErr.
			_ = tx.Rollback(context.WithoutCancel(t.Context()))
			tx = nil
		}
	}}
	var once sync.Once
	release = func() {
		once.Do(func() {
			<-pinned
			require.NoError(t, pinErr)
			if tx != nil {
				assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
			}
		})
	}
	t.Cleanup(func() {
		clock.once.Do(func() { close(pinned) })
		release()
	})
	return clock, release
}

// A run stopped in the middle of its copy resumes from the watermark its
// periodic save recorded, not from the start. One chunk is pinned by a
// concurrent writer so the watermark cannot pass it; the injected sleep is
// ticked until a save records the watermark just below the pinned chunk,
// and the run is stopped on that save. The pin is released, and the
// resumed run copies from the watermark to done.
func TestRunnerResumesMidCopyFromThePeriodicSave(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	clock, release := f.pinShadowKey(t, 150)
	const belowPin = 100

	ticks := make(chan struct{})
	tickedSleep := func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ticks:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var stoppedAt copier.Watermark
	var mu sync.Mutex
	stopBelowPin := func(_ context.Context, cp checkpoint.Checkpoint) error {
		if cp.Phase != checkpoint.PhaseCopying || cp.Watermark != copier.NewWatermark(belowPin) {
			return nil
		}
		mu.Lock()
		stoppedAt = cp.Watermark
		mu.Unlock()
		return errStopHere
	}
	// The pinned chunk waits on the pin's transaction for as long as the
	// copier's lock timeout allows; the wait must outlast the ticks that
	// reach the save under test, so the chunk waits rather than fails.
	const pinnedChunkWait = time.Minute
	opts := schemachange.RunOptions{
		Copier: copier.Options{
			Workers:     2,
			LockTimeout: pinnedChunkWait,
			Clock:       clock,
			Chunker:     copier.ChunkerOptions{InitialRows: 100, MinRows: 100, MaxRows: 100},
		},
		Checksum:           checksum.Options{Chunker: gateChunks},
		Sleep:              tickedSleep,
		CheckpointInterval: time.Hour,
		AfterCheckpoint:    stopBelowPin,
	}
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	target := f.prove(t, "orders")
	st := f.alter(t, alter)

	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := r.Run(t.Context(), f.pool, target, st)
		results <- err
	})
	t.Cleanup(wg.Wait)

	// Each tick asks the saver to write what has landed; the run stops on
	// the tick that finds the watermark just below the pinned chunk.
	const stopDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		select {
		case ticks <- struct{}{}:
		default:
		}
		mu.Lock()
		defer mu.Unlock()
		return stoppedAt.Valid()
	}, stopDeadline, 20*time.Millisecond, "a periodic save should record the watermark below the pinned chunk")
	// The failed save cancels the copy, so the run returns as soon as the
	// pinned chunk's wait is interrupted — well inside the chunk's own
	// lock timeout, which is the only other thing that would end it.
	const stopPropagation = 5 * time.Second
	select {
	case err := <-results:
		require.ErrorIs(t, err, errStopHere)
	case <-time.After(stopPropagation):
		t.Fatalf("run did not stop within %s of the failed save; the copy was not cancelled with it", stopPropagation)
	}
	row := f.inspectRow(t, "orders")
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase)
	assert.Equal(t, copier.NewWatermark(belowPin), row.Watermark, "the row carries the last save")
	release()

	// While nothing runs the source stays fenced: a writer that reaches it
	// is refused, so no change can slip in between the stop and the
	// resume. A shadow row above the watermark that disagrees with the
	// source stands for whatever the stopped copy left there, and is
	// cleared by the resumed copy before it lands that key space again.
	// The other worker runs ahead of the pinned chunk, so the stopped copy
	// may or may not have landed the key. The source's row is landed first
	// if it is not already there, so every run plants the stray value over
	// a landed row, the shape a fast server produces, and the read-back
	// fails if the plant does not change it.
	f.assertWriteFenced(t, "orders")
	shadow := pgx.Identifier{f.schema, row.ShadowTable}.Sanitize()
	f.exec(t, `
		INSERT INTO `+shadow+` (id, qty)
		SELECT id, qty
		FROM %s.orders
		WHERE id = 1500
		ON CONFLICT (id) DO NOTHING`)
	f.exec(t, `
		UPDATE `+shadow+`
		SET qty = 999
		WHERE id = 1500`)
	var strayQty int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+shadow+" WHERE id = 1500").Scan(&strayQty))
	require.Equal(t, 999, strayQty, "the shadow holds the stray value above the watermark before the resume")

	second := &stopAfter{}
	f.assertResumedToDone(t, alter, schemachange.RunOptions{AfterCheckpoint: second.hook})
	var qty int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" WHERE id = 1500").Scan(&qty))
	assert.Equal(t, 1500, qty, "the live table carries the source's row, not the stray shadow row")
	// The resumed copy's one copying save is the finished copy recording
	// its complete watermark before the verifying save.
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseCopying, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone}, second.seen(), "the resumed run finished the copy and went on through verification")
}

// A run cancelled in the middle of its copy records what the copy landed:
// the save after the copier returns outlives the cancelled context, so the
// row carries the watermark the chunks that committed reached, and the
// resumed run copies from there. The last chunk is pinned so the copy
// cannot finish, and the run is cancelled once every other chunk is in.
func TestRunnerRecordsTheWorkACancelledCopyLanded(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	clock, release := f.pinShadowKey(t, 2450)
	const landedBelowPin = 2400

	const pinnedChunkWait = time.Minute
	opts := schemachange.RunOptions{
		Copier: copier.Options{
			Workers:     2,
			LockTimeout: pinnedChunkWait,
			Clock:       clock,
			Chunker:     copier.ChunkerOptions{InitialRows: 100, MinRows: 100, MaxRows: 100},
		},
		Checksum:           checksum.Options{Chunker: gateChunks},
		CheckpointInterval: time.Hour,
	}
	r, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)
	target := f.prove(t, "orders")
	st := f.alter(t, alter)
	shadow := schemachange.ShadowName(f.schema, "orders")

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := r.Run(runCtx, f.pool, target, st)
		results <- err
	})
	t.Cleanup(wg.Wait)

	const landDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		return f.relationExists(t, shadow) && f.rowCount(t, shadow) == landedBelowPin
	}, landDeadline, 20*time.Millisecond, "every chunk below the pinned one should land")
	cancel()
	const cancelPropagation = 5 * time.Second
	select {
	case err := <-results:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(cancelPropagation):
		t.Fatalf("run did not return within %s of being cancelled", cancelPropagation)
	}

	row := f.inspectRow(t, "orders")
	assert.Equal(t, checkpoint.PhaseCopying, row.Phase)
	assert.Equal(t, copier.NewWatermark(landedBelowPin), row.Watermark, "the row records the watermark the committed chunks reached")
	release()

	second := &stopAfter{}
	f.assertResumedToDone(t, alter, schemachange.RunOptions{AfterCheckpoint: second.hook})
	assert.Equal(t, []checkpoint.Phase{checkpoint.PhaseCopying, checkpoint.PhaseVerifying, checkpoint.PhaseCutover, checkpoint.PhaseDone}, second.seen(), "the resumed run finished the copy from the recorded watermark")
}

// A quiesced run has no writer to explain a difference between source and
// shadow, so the verification pass runs under DivergenceAbort (CO-3): a
// shadow row changed under a stopped run stops the resumed run with the
// pass's report, the row stays in verifying, and nothing is swapped. The
// operator drops the shadow, takes the fence off, and deletes the row to
// start over.
func TestRunnerStopsOnAShadowThatDiffersFromTheSource(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	sourceOID := f.relationOID(t, "orders")
	stop := &stopAfter{phase: checkpoint.PhaseVerifying}

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{AfterCheckpoint: stop.hook})
	require.ErrorIs(t, err, errStopHere)
	row := f.inspectRow(t, "orders")
	shadow := pgx.Identifier{f.schema, row.ShadowTable}.Sanitize()
	f.exec(t, "UPDATE "+shadow+" SET qty = 0 WHERE id = 5")

	result, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	var divergence *checksum.DivergenceError
	require.ErrorAs(t, err, &divergence)
	assert.Len(t, divergence.Report.Mismatches, 1, "one chunk differs")
	assert.Equal(t, 3, divergence.Report.Chunks, "the pass read the whole key space")
	assert.Zero(t, result, "a run that stopped hands back no result")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t, "orders").Phase, "the row stays where the pass found the difference")
	assert.Equal(t, sourceOID, f.relationOID(t, "orders"), "nothing was swapped")
	assert.True(t, f.hasFence(t, "orders"), "the source stays fenced for the operator to abandon the run")
}
