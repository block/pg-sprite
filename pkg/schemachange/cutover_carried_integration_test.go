package schemachange_test

import (
	"context"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
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

// carriedKey is the row the swaps below are left to compare.
const carriedKey int64 = 7

// leaveKeyForSwap copies the fixture's table into shadow and runs a live
// pass that mints a proof leaving carriedKey for the swap. No catch-up
// runs, so the shadow never changes, and the source row is rewritten
// before the pass and before each reread: the pass, allowed to leave one
// key, cannot judge it and names it in the proof. It returns the cutover
// gate's proof and the amount the source now holds for the key, which the
// shadow does not.
func leaveKeyForSwap(t *testing.T, f reconcileFixture, shadow schemachange.BuiltShadow) (schemachange.CutoverReady, string) {
	t.Helper()
	copyAll(t, f, newCopier(t, f, shadow))
	rewrite := func() {
		_, err := f.pool.Exec(t.Context(), `
			UPDATE `+f.table.Qualified()+`
			SET amount = amount + 1
			WHERE id = $1`, carriedKey)
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
	require.Equal(t, []int64{carriedKey}, verified.Carried(), "the proof leaves the key for the swap")

	ready, err := schemachange.GateCutover(t.Context(), f.pool, f.lock, shadow, verified, schemachange.Options{})
	require.NoError(t, err, "the gate accepts a proof that leaves keys; the swap compares them")

	var onSource, onShadow string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT amount::text
		FROM `+f.table.Qualified()+`
		WHERE id = $1`, carriedKey).Scan(&onSource))
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT amount::text
		FROM `+shadowName(f, shadow)+`
		WHERE id = $1`, carriedKey).Scan(&onShadow))
	require.NotEqual(t, onSource, onShadow, "the shadow holds an earlier amount for the key")
	return ready, onSource
}

// shadowName is the shadow's schema-qualified, quoted name.
func shadowName(f reconcileFixture, shadow schemachange.BuiltShadow) string {
	return pgx.Identifier{f.table.Schema, shadow.ShadowTable()}.Sanitize()
}

// A proof that leaves a key for the swap is finished inside the swap
// transaction: after the drain, under ACCESS EXCLUSIVE on both tables, the
// swap reads the key on the source and on the shadow. The drain here
// stands in for the catch-up's and applies the one change the stream would
// carry, the source's latest amount for the key, so the two sides are
// equal, the swap commits, and the live table holds that amount.
func TestCutoverSwapsWhenTheDrainLevelsTheKeysThePassLeft(t *testing.T) {
	f := newReconcileFixture(t, 300)
	shadow := f.buildShadow(t)
	ready, want := leaveKeyForSwap(t, f, shadow)
	drain := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE `+shadowName(f, shadow)+` AS held
			SET amount = source.amount
			FROM `+f.table.Qualified()+` AS source
			WHERE source.id = held.id
			  AND held.id = $1`, carriedKey)
		return err
	}

	swapped, err := schemachange.Cutover(t.Context(), f.pool, f.lock, ready, drain, schemachange.CutoverOptions{})
	require.NoError(t, err)

	assert.Equal(t, f.table.Table, swapped.Table())
	var live bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid = $2`, f.table.Qualified(), shadow.ShadowOID()).Scan(&live))
	assert.True(t, live, "the shadow bears the source's name")
	var got string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT amount::text
		FROM `+f.table.Qualified()+`
		WHERE id = $1`, carriedKey).Scan(&got))
	assert.Equal(t, want, got, "the live table holds the source's latest amount for the key")
}

// With a drain that applies nothing, the key the pass left still differs
// under the swap lock: the shadow would go live holding an amount the
// source has since replaced. The swap is refused as
// cutover-carried-diverged (CO-1), an invariant violation, and rolled
// back: the source still bears its name and still holds its amount.
func TestCutoverRefusesWhenAKeyThePassLeftStillDiffersAfterTheDrain(t *testing.T) {
	f := newReconcileFixture(t, 300)
	shadow := f.buildShadow(t)
	ready, want := leaveKeyForSwap(t, f, shadow)
	drain := func(context.Context, pgx.Tx) error { return nil }

	_, err := schemachange.Cutover(t.Context(), f.pool, f.lock, ready, drain, schemachange.CutoverOptions{})
	require.Error(t, err)

	assert.Equal(t, schemachange.CauseCarriedDiverged, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.ErrorIs(t, err, schemachange.ErrCutoverRolledBack, "a refused swap says the source is live")
	var live bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid = $2`, f.table.Qualified(), shadow.SourceOID()).Scan(&live))
	assert.True(t, live, "the source still bears its name")
	var got string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT amount::text
		FROM `+f.table.Qualified()+`
		WHERE id = $1`, carriedKey).Scan(&got))
	assert.Equal(t, want, got, "the source row is as it was")
}
