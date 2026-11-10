package schemachange_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// rewritingPosition stands in for the catch-up's position: it claims
// everything is confirmed and rewrites one source row first, so that row
// reads changed at every reread of a live pass.
type rewritingPosition func()

func (rewrite rewritingPosition) Confirmed() decode.LSN {
	rewrite()
	return math.MaxUint64
}

// A verified-shadow proof that names keys the pass could not judge proves
// every key but those, and leaves them for the swap to compare under its
// lock. This swap compares none, so the gate refuses the proof as
// cutover-unverified (CO-1) and the source stays live. No catch-up runs
// here, so the shadow never changes, and source row 7 is rewritten before
// the pass and before each reread: the pass, allowed to leave one key,
// mints a proof naming it.
func TestGateCutoverRefusesAProofThatLeavesKeysForTheSwap(t *testing.T) {
	f := newReconcileFixture(t, 300)
	shadow := f.buildShadow(t)
	copyAll(t, f, newCopier(t, f, shadow))
	rewrite := func() {
		_, err := f.pool.Exec(t.Context(), `UPDATE `+f.table.Qualified()+` SET amount = amount + 1 WHERE id = 7`)
		require.NoError(t, err)
	}
	rewrite()

	v, err := checksum.NewVerifier(f.target, shadow, f.lock, checksum.Options{})
	require.NoError(t, err)
	outcome, err := v.CheckLive(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed: rewritingPosition(rewrite),
		Attempts:  2,
		CarryKeys: 1,
	})
	require.NoError(t, err)
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted)
	require.Equal(t, []int64{7}, verified.Carried(), "the proof leaves key 7 for the swap")

	_, err = schemachange.GateCutover(t.Context(), f.pool, f.lock, shadow, verified, schemachange.Options{})
	require.Error(t, err)
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err))
	var live bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid = $2`, f.table.Qualified(), shadow.SourceOID()).Scan(&live))
	assert.True(t, live, "a refused gate leaves the source live")
}
