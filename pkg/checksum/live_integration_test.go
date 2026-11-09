package checksum_test

import (
	"context"
	"errors"
	"math"
	"strconv"
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
	verified, minted := outcome.VerifiedShadow()
	assert.True(t, minted)
	assert.True(t, verified.Live(), "the proof says a live pass minted it")
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), work.ChunksMismatched, "the sweep found one chunk differing")
	assert.Equal(t, uint64(1), work.ChunksReread, "settling the chunk took one reread")
	f.assertConverged(t)
}

// A chunk whose shadow has reached exactly what the sweep saw on the
// source, while the source has moved on again, is settled: the stream has
// delivered what the first read saw, and the newer change is still in the
// stream. The first change is held off the shadow for the sweep; the hold
// is released from inside the pass's first wait, the change allowed to
// land and the catch-up to confirm past the sweep's read, then the shadow
// is held again and the source changed once more, so the reread finds the
// shadow equal to the sweep's source and the source elsewhere. The
// catch-up confirms no further than the position the stream had reached
// before a change it has yet to apply, so the second change is made only
// once the catch-up has confirmed past the read it must settle.
func TestCheckLiveSettlesAChunkWhoseShadowReachedTheSweepsSource(t *testing.T) {
	f := newLiveFixture(t)
	release := f.holdShadow(t)
	touch := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 1500"
	f.exec(t, touch)

	var waits atomic.Int32
	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Sleep: func(ctx context.Context, d time.Duration) error {
			if waits.Add(1) > 1 {
				return pollSleep(ctx, d)
			}
			release()
			require.Eventually(t, func() bool {
				var same bool
				require.NoError(t, f.pool.QueryRow(ctx, "SELECT s.amount = t.amount FROM "+f.table.Qualified()+" s, "+f.shadowName()+" t WHERE s.id = 1500 AND t.id = 1500").Scan(&same))
				return same
			}, 10*time.Second, 20*time.Millisecond, "the first change landed on the shadow")
			marked := f.currentWALLSN(t)
			require.Eventually(t, func() bool { return f.catchup.Confirmed() >= marked },
				10*time.Second, 20*time.Millisecond, "the catch-up confirmed past the sweep's read")
			f.holdShadow(t)
			f.exec(t, touch)
			return nil
		},
	})
	require.NoError(t, err)
	assert.True(t, outcome.Clean())
	assert.Equal(t, 1, outcome.Report.Settled, "the shadow reached the sweep's source while the source moved on")
	assert.Empty(t, outcome.Report.Mismatches)
}

// A chunk written between every read settles when each of its keys does,
// though the chunk as a whole never holds still: the shadow does not
// reach the state the sweep saw on the source, because rows the sweep
// read equal have moved on both sides since, and the source does not
// match the shadow, because another row changed after the catch-up
// confirmed. Judged key by key, every row is accounted for. Three rows of
// the first chunk stand for the three ways a key under load settles: the
// first is changed before the sweep and lands on the shadow before the
// reread, so the reread finds it equal; the second is changed after the
// sweep and lands too, so it is equal as well; the third is changed after
// the catch-up confirmed and is held off the shadow, so the reread finds
// it on the shadow as the sweep saw it on the source while the source
// holds the newer value the stream still carries. Nothing was divergent,
// so the chunk settles on the first reread, the pass is clean, and the
// proofs are minted; once the hold is released the stream delivers the
// third change and the tables converge.
func TestCheckLiveSettlesAChunkWrittenBetweenEveryRead(t *testing.T) {
	f := newLiveFixture(t)
	release := f.holdShadow(t)
	touch := func(id int) string {
		return "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = " + strconv.Itoa(id)
	}
	f.exec(t, touch(1))

	var rehold func()
	var waits atomic.Int32
	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Sleep: func(ctx context.Context, d time.Duration) error {
			if waits.Add(1) > 1 {
				return pollSleep(ctx, d)
			}
			release()
			f.exec(t, touch(2))
			require.Eventually(t, func() bool {
				var same bool
				require.NoError(t, f.pool.QueryRow(ctx, "SELECT bool_and(s.amount = t.amount) FROM "+f.table.Qualified()+" s JOIN "+f.shadowName()+" t ON t.id = s.id WHERE s.id IN (1, 2)").Scan(&same))
				return same
			}, 10*time.Second, 20*time.Millisecond, "the first two changes landed on the shadow")
			marked := f.currentWALLSN(t)
			require.Eventually(t, func() bool { return f.catchup.Confirmed() >= marked },
				10*time.Second, 20*time.Millisecond, "the catch-up confirmed past the sweep's read")
			rehold = f.holdShadow(t)
			f.exec(t, touch(3))
			return nil
		},
	})
	require.NoError(t, err)
	assert.True(t, outcome.Clean())
	assert.Equal(t, 3, outcome.Report.Chunks)
	assert.Equal(t, 1, outcome.Report.Settled, "every key of the chunk settled on the first reread")
	assert.Equal(t, 0, outcome.Report.Unsettled)
	assert.Empty(t, outcome.Report.Mismatches)
	_, minted := outcome.VerifiedShadow()
	assert.True(t, minted, "a pass whose every differing chunk settled verifies the shadow")
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), work.ChunksReread, "a chunk whose keys all settled is not read again")

	require.NotNil(t, rehold, "the pass waited on the catch-up")
	rehold()
	require.Eventually(t, func() bool {
		var same bool
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT s.amount = t.amount FROM "+f.table.Qualified()+" s JOIN "+f.shadowName()+" t ON t.id = s.id WHERE s.id = 3").Scan(&same))
		return same
	}, 10*time.Second, 20*time.Millisecond, "the stream delivered the change the reread saw still in flight")
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
	assert.Equal(t, 1, unresolved.Keys, "only key 1 kept changing")
	assert.Equal(t, chunk(t, math.MinInt64, 1000), unresolved.Chunk)
	assert.Equal(t, int64(1000), unresolved.Source.Rows)
	assert.Equal(t, int64(1000), unresolved.Shadow.Rows)
	assert.NotEqual(t, unresolved.Source.Hash, unresolved.Shadow.Hash)
	assert.Empty(t, outcome.Repairs, "an unsettled chunk is not repaired")
	assertNoProofs(t, outcome)
}

// A key settled at one reread is concluded as of that read, so the rereads
// after it judge only the keys left hot. The shadow is held still and the
// catch-up's position stands in claiming everything is confirmed. Key 1
// is changed before the pass, so the sweep finds the first chunk
// differing, and again every time the pass asks for the position, so it
// is hot at every reread. Key 2 is changed once, after the sweep: the
// first reread finds the shadow holding what the sweep saw on the source,
// which settles it. Judged again at the second reread against the first,
// key 2 would look still on both sides and differing, a divergence the
// stream has not been given the chance to deliver; judged only from the
// hot keys, the chunk ends unsettled with one key still changing.
func TestCheckLiveJudgesOnlyTheKeysStillHotAfterTheFirstReread(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	hot := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 1"
	f.exec(t, hot)
	asked := 0
	rewrite := confirmedFunc(func() decode.LSN {
		if asked == 0 {
			f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 2")
		}
		asked++
		f.exec(t, hot)
		return math.MaxUint64
	})

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{Confirmed: rewrite, Attempts: 2})
	require.ErrorIs(t, err, checksum.ErrVerificationUnresolved, "key 2 settled at the first reread and is not judged again")
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseHot, unresolved.Cause)
	assert.Equal(t, 2, unresolved.Rereads)
	assert.Equal(t, 1, unresolved.Keys, "only key 1 was still changing")
	assert.Equal(t, chunk(t, math.MinInt64, 1000), unresolved.Chunk)
	assert.Empty(t, outcome.Report.Mismatches)
	assertNoProofs(t, outcome)
}

// The first reread judges every key either read holds on either side,
// including a key only the reread holds. Key 10 is changed with the shadow
// held, so the sweep finds the first chunk differing; at the pass's first
// wait the hold is released, the change lands and the catch-up confirms
// past the sweep, and then a row the source never held is written
// straight into the shadow under key 0. The reread finds key 10 equal and
// key 0 moved on the shadow, so it is hot; the second reread finds key 0
// still on both sides and still differing, a confirmed divergence. Judged
// only from the sweep's keys, key 0 would never be judged and the pass
// would verify a shadow holding a row the source does not.
func TestCheckLiveJudgesAShadowRowThatAppearedAfterTheSweep(t *testing.T) {
	f := newLiveFixture(t)
	release := f.holdShadow(t)
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 10")

	var waits atomic.Int32
	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Sleep: func(ctx context.Context, d time.Duration) error {
			if waits.Add(1) > 1 {
				return pollSleep(ctx, d)
			}
			release()
			require.Eventually(t, func() bool {
				var same bool
				require.NoError(t, f.pool.QueryRow(ctx, "SELECT s.amount = t.amount FROM "+f.table.Qualified()+" s, "+f.shadowName()+" t WHERE s.id = 10 AND t.id = 10").Scan(&same))
				return same
			}, 10*time.Second, 20*time.Millisecond, "the change to key 10 landed on the shadow")
			marked := f.currentWALLSN(t)
			require.Eventually(t, func() bool { return f.catchup.Confirmed() >= marked },
				10*time.Second, 20*time.Millisecond, "the catch-up confirmed past the sweep's read")
			f.exec(t, `
				INSERT INTO `+f.shadowName()+` (
					id,
					uniq,
					amount
				) VALUES (
					0,
					0,
					1.00
				)`)
			return nil
		},
	})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged, "the row the shadow gained after the sweep is a divergence")
	require.Len(t, outcome.Report.Mismatches, 1)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), outcome.Report.Mismatches[0].Chunk)
	assert.Equal(t, int64(1000), outcome.Report.Mismatches[0].Source.Rows)
	assert.Equal(t, int64(1001), outcome.Report.Mismatches[0].Shadow.Rows, "the shadow holds key 0 beside keys 1..1000")
	assertNoProofs(t, outcome)
}

// A live pass holds the rows of a few differing chunks at most: once it
// holds that many it settles them before it reads on, so the rows it holds
// stay bounded however many chunks of the table differ. Every shadow row
// is written by something other than the stream, so every one of the
// pass's hundred-row chunks holding keys 1..2500 differs in the sweep, and
// the last chunk, open above 2500, holds no row on either side and is
// equal. Every time the pass asks for the catch-up's position the test
// records how many chunks the sweep has found differing and how many
// rereads the pass has made; each chunk here is a confirmed divergence on
// its one reread, so the difference is the number of differing chunks
// held and not yet settled. It reaches four and never passes it, and the
// pass still settles every differing chunk and reports each divergence.
func TestCheckLiveHoldsAFewDifferingChunksAtATime(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0")

	v, err := checksum.NewVerifier(f.target, f.shadow, f.lock, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MinRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	var mostHeld int64
	watch := confirmedFunc(func() decode.LSN {
		work, err := v.Work(t.Context())
		require.NoError(t, err)
		mostHeld = max(mostHeld, int64(work.ChunksMismatched)-int64(work.ChunksReread))
		return f.catchup.Confirmed()
	})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{Confirmed: watch})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged)
	assert.Equal(t, 26, outcome.Report.Chunks, "keys 1..2500 in hundred-row chunks, and the empty chunk above them")
	require.Len(t, outcome.Report.Mismatches, 25, "every chunk holding rows is a confirmed divergence")
	assert.Equal(t, chunk(t, math.MinInt64, 100), outcome.Report.Mismatches[0].Chunk)
	assert.Equal(t, chunk(t, 2401, 2500), outcome.Report.Mismatches[24].Chunk)
	assert.Equal(t, int64(4), mostHeld, "the sweep held at most four differing chunks before settling them")
	work, err := v.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(25), work.ChunksReread, "each differing chunk was settled on its one reread")
	assertNoProofs(t, outcome)
}

// A catch-up that does not confirm past the position a differing chunk
// was read at within the settle wait leaves the chunk unsettled as
// lagging: the pass cannot tell a slow stream from a divergence and says
// so instead of guessing either way. The shadow is held so the catch-up
// stays below the change, and the pass's sleep returns at once, so the
// wait runs through its polls without wall time passing.
func TestCheckLiveReportsALaggingCatchupAsUnresolved(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 1500")

	var polls atomic.Int32
	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		SettleWait: time.Second,
		Poll:       100 * time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			polls.Add(1)
			return nil
		},
	})
	assert.Equal(t, int32(10), polls.Load(), "the wait is as many polls as fit in the settle wait")
	require.ErrorIs(t, err, checksum.ErrVerificationUnresolved)
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseLagging, unresolved.Cause)
	assert.Zero(t, unresolved.Rereads, "the pass gave up before its first reread")
	assert.Equal(t, chunk(t, 1001, 2000), unresolved.Chunk)
	assert.Less(t, unresolved.Confirmed, unresolved.Captured, "the catch-up had not reached the read's position")
	assertNoProofs(t, outcome)
}

// A session holding a transaction open after writing the source leaves
// WAL behind its last record that nothing flushes, and the walsender
// cannot read past the flushed position. The position a live pass marks
// a read with is flushed regardless, so a stream that has delivered
// everything confirms past the read and the pass reaches a verdict: here
// a shadow row the stream will never fix is a divergence, not a lagging
// catch-up.
func TestCheckLiveConfirmsPastAnOpenWritersWAL(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1500")

	writer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the test rolls the writer back itself.
		_ = writer.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = writer.Exec(t.Context(), "UPDATE "+f.table.Qualified()+" SET amount = amount WHERE id = 5")
	require.NoError(t, err)

	v := f.verifier(t, checksum.Options{})
	_, err = f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{SettleWait: 3 * time.Second})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged, "the stream is caught up, so the shadow row is a divergence, not a lagging catch-up")
	require.Len(t, diverged.Report.Mismatches, 1)
	assert.Equal(t, chunk(t, 1001, 2000), diverged.Report.Mismatches[0].Chunk)
	require.NoError(t, writer.Rollback(t.Context()))
}

// A chunk that will not settle does not hide a divergence in another: the
// pass settles every differing chunk before it decides. The second chunk
// holds a shadow row the stream will never fix and the third is rewritten
// on every poll so it stays hot; the pass reports the confirmed divergence
// with the unsettled chunk counted beside it, and mints nothing.
func TestCheckLiveReportsADivergenceAlongsideAnUnsettledChunk(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1500")
	f.holdShadow(t)
	touch := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 2400"
	f.exec(t, touch)
	rewrite := confirmedFunc(func() decode.LSN {
		f.exec(t, touch)
		return math.MaxUint64
	})

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{Confirmed: rewrite, Attempts: 2})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged)
	require.Len(t, outcome.Report.Mismatches, 1, "the divergence in chunk [1001, 2000] is confirmed and reported")
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Report.Mismatches[0].Chunk)
	assert.Equal(t, 1, outcome.Report.Unsettled, "the hot chunk [2001, max] is counted, not dropped")
	assert.False(t, outcome.Clean())
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
