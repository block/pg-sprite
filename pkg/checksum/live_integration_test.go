package checksum_test

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// The live passes below run against the engine's real catch-up stage:
// the source is written while the pass runs and the stream carries the
// writes to the shadow. Each test aims one timing at the pass — a shadow
// the stream has not yet reached, a chunk that will not hold still, a
// catch-up that never confirms, a shadow written by something other than
// the stream — and checks the pass tells them apart.

// A source change the stream has not yet applied is not a divergence. The
// shadow is held so the flush waits; the sweep then reads the changed
// chunk differing, records the position it read at, and waits for the
// catch-up, which cannot confirm past that position until the hold ends.
// The hold is released from inside the pass's wait, the flush lands, the
// reread finds both sides equal, and the pass is clean: the chunk counts as
// settled, not mismatched, and both proofs are minted (CO-1). The update
// leaves the out-of-line blob column unchanged, so the change record the
// stream carries omits it and the flush must keep the copied value.
func TestCheckLiveSettlesAChunkTheStreamHadNotYetApplied(t *testing.T) {
	f := newLiveFixture(t)
	release := f.holdShadow(t)
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id <= 10")

	var sleeps atomic.Int32
	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps.Add(1)
			release()
			return pollSleep(ctx, d)
		},
	})
	require.NoError(t, err)
	assert.True(t, outcome.Clean())
	assert.Equal(t, 1, outcome.Report.Settled, "the chunk holding keys 1..10 settled")
	assert.Empty(t, outcome.Report.Mismatches)
	assert.Equal(t, 3, outcome.Report.Chunks)
	assert.Positive(t, sleeps.Load(), "the pass waited for the catch-up before rereading")
	_, minted := outcome.CleanWatermark()
	assert.True(t, minted)
	_, minted = outcome.VerifiedShadow()
	assert.True(t, minted)
	f.assertConverged(t)
}

// A chunk whose source changes between every read cannot be judged: no
// two reads agree on what the source held, so the shadow can neither be
// behind nor ahead of a known state. The shadow is held still, a source
// row in the first chunk is changed before the pass so the sweep finds
// the chunk differing, and the same row is rewritten every time the pass
// asks for the confirmed position, which here claims everything is
// confirmed, so every reread finds a moved source against an unchanged
// shadow. After the
// stated attempts the pass reports the chunk unsettled as hot, with the
// reread count it spent, and mints no proof; the shadow is as the pass
// found it.
func TestCheckLiveReportsAChunkThatKeepsChangingAsUnresolved(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	touch := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 1"
	f.exec(t, touch)
	rewrite := confirmedFunc(func() decode.LSN {
		f.exec(t, touch)
		return math.MaxUint64
	})

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{Confirmed: rewrite, Attempts: 2})
	require.ErrorIs(t, err, checksum.ErrVerificationUnresolved)
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseHot, unresolved.Cause)
	assert.Equal(t, 2, unresolved.Rereads)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), unresolved.Chunk)
	assert.Equal(t, int64(1000), unresolved.Source.Rows)
	assert.Equal(t, int64(1000), unresolved.Shadow.Rows)
	assert.NotEqual(t, unresolved.Source.Hash, unresolved.Shadow.Hash)
	assert.Empty(t, outcome.Repairs, "an unsettled chunk is not repaired")
	assertNoProofs(t, outcome)
}

// A catch-up that does not confirm past the position a differing chunk
// was read at within the settle wait leaves the chunk unsettled as
// lagging: the pass cannot tell a slow stream from a divergence and says
// so instead of guessing either way. The shadow is held so the catch-up
// stays below the change, and the verifier's clock steps past the wait
// without wall time passing.
func TestCheckLiveReportsALaggingCatchupAsUnresolved(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 1500")

	v := f.verifier(t, checksum.Options{Clock: &steppingClock{now: time.Unix(0, 0), step: time.Second}})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		SettleWait: time.Second,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	require.ErrorIs(t, err, checksum.ErrVerificationUnresolved)
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseLagging, unresolved.Cause)
	assert.Zero(t, unresolved.Rereads, "the pass gave up before its first reread")
	assert.Equal(t, chunk(t, 1001, 2000), unresolved.Chunk)
	assert.Less(t, unresolved.Confirmed, unresolved.Captured, "the catch-up had not reached the read's position")
	assertNoProofs(t, outcome)
}

// A shadow row written by something other than the stream stays wrong
// after the catch-up has confirmed past the sweep: neither side moves
// across the confirmed position and they still differ, so the pass
// reports the divergence. Under DivergenceAbort it names the chunk and
// leaves the shadow as it found it; DivergenceRecopy is refused outright,
// since a live pass settles what it recopies.
func TestCheckLiveAbortsOnAConfirmedDivergence(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1500")

	v := f.verifier(t, checksum.Options{})
	_, err := f.checkLive(t, v, checksum.DivergenceRecopy, checksum.LiveOptions{})
	assert.ErrorIs(t, err, checksum.ErrNoDivergencePolicy)

	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged)
	require.Len(t, diverged.Report.Mismatches, 1)
	assert.Equal(t, chunk(t, 1001, 2000), diverged.Report.Mismatches[0].Chunk)
	assert.Zero(t, outcome.Report.Settled)
	assertNoProofs(t, outcome)

	var amount string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT amount::text FROM "+f.shadowName()+" WHERE id = 1500").Scan(&amount))
	assert.Equal(t, "0.00", amount, "an aborting pass does not write the shadow")
}

// Under DivergenceRepair a confirmed divergence is recopied from the
// source while the stream keeps writing the shadow: the recopy fences the
// flushes out, the pass waits for the catch-up to confirm past the recopy,
// and the chunk is read clean. A source change to the same chunk committed
// just before the pass is carried by the stream too, and the flush that
// replays it after the recopy puts back the image the recopy already
// holds. The repairing pass mints no proof (CO-2); the next pass reads
// every chunk fresh and mints both.
func TestCheckLiveRepairsAConfirmedDivergenceUnderTheStream(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 1500")

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{})
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 1)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, int64(1000), outcome.Repairs[0].Removed)
	assert.Equal(t, int64(1000), outcome.Repairs[0].Inserted)
	assertNoProofs(t, outcome)
	f.assertConverged(t)

	again, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{})
	require.NoError(t, err)
	assert.True(t, again.Clean())
	assert.Empty(t, again.Repairs)
	_, minted := again.VerifiedShadow()
	assert.True(t, minted)
}

// A pass whose context ends while it waits for the catch-up returns the
// context's error, not an unsettled chunk: the caller stopped it, and
// nothing about the data was decided.
func TestCheckLiveStopsWithItsContextWhileWaiting(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 1")

	ctx, cancel := context.WithCancel(t.Context())
	v := f.verifier(t, checksum.Options{})
	_, err := v.CheckLive(ctx, f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed: f.catchup,
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	})
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.False(t, errors.Is(err, checksum.ErrVerificationUnresolved))
}
