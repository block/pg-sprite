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

// A verified-shadow proof a live pass minted says the shadow equals the
// source as of a position the stream had reached, with every change since
// still in the stream. The cutover gate accepts it like a quiesced proof,
// and the swap then insists on a drain to apply those changes under its
// ACCESS EXCLUSIVE lock: with none it is refused as cutover-undrained
// (CO-1) and the source stays live; with a drain it swaps. The stream is
// stopped before the swap, as an orchestrator's drain would have consumed
// it, so the no-op drain here has nothing left to apply.
func TestCutoverRequiresADrainForALiveProof(t *testing.T) {
	f := newReconcileFixture(t, 300)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	c := newCopier(t, f, shadow)
	cap := f.startCapture(t, shadow, c, slot.ConsistentPoint())
	copyAll(t, f, c)

	v, err := checksum.NewVerifier(f.target, shadow, f.lock, checksum.Options{})
	require.NoError(t, err)
	outcome, err := v.CheckLive(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort, checksum.LiveOptions{Confirmed: cap.catchup})
	require.NoError(t, err)
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted)
	require.True(t, verified.Live(), "the proof says a live pass minted it")

	ready, err := schemachange.GateCutover(t.Context(), f.pool, f.lock, shadow, verified, schemachange.Options{})
	require.NoError(t, err, "the gate accepts a live proof; the swap decides what it needs")

	_, err = schemachange.Cutover(t.Context(), f.pool, f.lock, ready, nil, schemachange.CutoverOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseCutoverUndrained, schemachange.RefusalCauseOf(err))
	var live bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid = $2`, f.table.Qualified(), shadow.SourceOID()).Scan(&live))
	assert.True(t, live, "a refused swap leaves the source live")

	cap.stop(t)
	drained := false
	drain := func(context.Context, pgx.Tx) error {
		drained = true
		return nil
	}
	swapped, err := schemachange.Cutover(t.Context(), f.pool, f.lock, ready, drain, schemachange.CutoverOptions{})
	require.NoError(t, err)
	assert.True(t, drained, "the drain ran under the swap lock")
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid = $2`, f.table.Qualified(), shadow.ShadowOID()).Scan(&live))
	assert.True(t, live, "the shadow bears the source's name")
	assert.Equal(t, swapped.Table(), f.table.Table)
}
