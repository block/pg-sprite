package checksum_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/decode"
)

// The passes below are told they may leave keys for the swap to compare.
// Each holds the shadow still and has a stand-in for the catch-up's
// position rewrite the same source rows every time the pass asks for it,
// so those keys read changed at every reread and their chunks run out of
// attempts.

// touchOnEveryAsk returns a stand-in for the catch-up's position that
// claims everything is confirmed and runs each statement first, so the
// rows the statements write are changed again before every reread.
func touchOnEveryAsk(t *testing.T, f *liveFixture, statements ...string) confirmedFunc {
	t.Helper()
	return func() decode.LSN {
		for _, sql := range statements {
			f.exec(t, sql)
		}
		return math.MaxUint64
	}
}

// A pass that may leave two keys and finds exactly two that never hold
// still, one in the first chunk and one in the third, returns no error
// and mints a live verified shadow naming them in key order. Nothing else
// about the pass is relaxed: the two chunks stay counted unsettled, the
// outcome is not clean, no clean watermark is minted, and the shadow is
// not written.
func TestCheckLiveLeavesKeysThatKeepChangingForTheSwap(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	first := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 1"
	third := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 2400"
	f.exec(t, first)
	f.exec(t, third)

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{
		Confirmed: touchOnEveryAsk(t, f, first, third),
		Attempts:  2,
		CarryKeys: 2,
	})
	require.NoError(t, err)

	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted, "the pass judged every key but the two it names")
	assert.True(t, verified.Live())
	assert.Equal(t, []int64{1, 2400}, verified.Carried())
	assert.True(t, verified.Watermark().Complete())
	assert.Equal(t, f.shadow.ShadowTable(), verified.Shadow())

	_, minted = outcome.CleanWatermark()
	assert.False(t, minted, "two chunks were not read clean")
	assert.False(t, outcome.Clean())
	assert.Equal(t, 2, outcome.Report.Unsettled)
	assert.Equal(t, 2, outcome.Report.Carried)
	assert.Empty(t, outcome.Report.Mismatches)
	assert.Empty(t, outcome.Repairs, "a chunk left for the swap is not repaired")
}

// The same two keys against a pass that may leave only one: the pass is
// over what the swap would compare, so it leaves nothing and reports the
// first chunk it gave up on, as a pass that may leave no key does.
func TestCheckLiveLeavesNoKeyWhenMoreKeepChangingThanItMayLeave(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	first := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 1"
	third := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 2400"
	f.exec(t, first)
	f.exec(t, third)

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{
		Confirmed: touchOnEveryAsk(t, f, first, third),
		Attempts:  2,
		CarryKeys: 1,
	})
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseHot, unresolved.Cause)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), unresolved.Chunk)
	assert.Equal(t, 2, outcome.Report.Unsettled)
	assert.Zero(t, outcome.Report.Carried)
	assertNoProofs(t, outcome)
}

// A divergence confirmed in another chunk is still the pass's answer: the
// second chunk holds a shadow row the stream will never fix, the third is
// kept changing, and a pass that may leave that one key reports the
// divergence and mints nothing.
func TestCheckLiveReportsADivergenceRatherThanLeavingKeysForTheSwap(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1500")
	f.holdShadow(t)
	touch := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 2400"
	f.exec(t, touch)

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed: touchOnEveryAsk(t, f, touch),
		Attempts:  2,
		CarryKeys: 1,
	})
	var diverged *checksum.DivergenceError
	require.ErrorAs(t, err, &diverged)
	require.Len(t, outcome.Report.Mismatches, 1)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Report.Mismatches[0].Chunk)
	assert.Equal(t, 1, outcome.Report.Unsettled)
	assert.Zero(t, outcome.Report.Carried)
	assertNoProofs(t, outcome)
}

// A chunk the catch-up never confirmed past has no key the swap could
// compare in its place: none of its keys was judged. The first chunk is
// changed once and the stand-in position stays below every read through
// that chunk's wait, which asks for it twice, before its one sleep and
// after; from then on it claims everything is confirmed and keeps key
// 2400 changing. A pass that may leave that one key still reports the
// lagging chunk and mints nothing.
func TestCheckLiveLeavesNoKeyWhenAnotherChunkIsLagging(t *testing.T) {
	f := newLiveFixture(t)
	f.holdShadow(t)
	touch := "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 2400"
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 500")
	f.exec(t, touch)
	const asksInTheFirstWait = 2
	asked := 0
	position := confirmedFunc(func() decode.LSN {
		asked++
		if asked <= asksInTheFirstWait {
			return 0
		}
		f.exec(t, touch)
		return math.MaxUint64
	})

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed:  position,
		Attempts:   2,
		CarryKeys:  1,
		SettleWait: 100 * time.Millisecond,
		Poll:       100 * time.Millisecond,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	var unresolved *checksum.UnresolvedError
	require.ErrorAs(t, err, &unresolved)
	assert.Equal(t, checksum.CauseLagging, unresolved.Cause)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), unresolved.Chunk)
	assert.Equal(t, 2, outcome.Report.Unsettled, "the lagging chunk and the one kept changing")
	assert.Zero(t, outcome.Report.Carried)
	assertNoProofs(t, outcome)
}
