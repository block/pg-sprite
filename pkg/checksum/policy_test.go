package checksum

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A pass must state one of the two named policies; the zero value is no
// policy, and a string that happens to type-check is not one either (CO-3).
func TestDivergencePolicyValidate(t *testing.T) {
	require.NoError(t, DivergenceAbort.validate())
	require.NoError(t, DivergenceRepair.validate())

	err := DivergencePolicy("").validate()
	require.ErrorIs(t, err, ErrNoDivergencePolicy)
	assert.EqualError(t, err, `no divergence policy: a pass must state "abort" or "repair"`)

	err = DivergencePolicy("Abort").validate()
	require.ErrorIs(t, err, ErrNoDivergencePolicy, "policies are exact, not case-folded")
	assert.EqualError(t, err, `no divergence policy: "Abort" is not "abort" or "repair"`)
}

// A caller loading the policy from configuration gets the typed value for
// the two names and the same refusal Check would give for anything else,
// so a wrong setting is refused before any copy runs.
func TestParseDivergencePolicy(t *testing.T) {
	policy, err := ParseDivergencePolicy("abort")
	require.NoError(t, err)
	assert.Equal(t, DivergenceAbort, policy)
	policy, err = ParseDivergencePolicy("repair")
	require.NoError(t, err)
	assert.Equal(t, DivergenceRepair, policy)

	policy, err = ParseDivergencePolicy("")
	require.ErrorIs(t, err, ErrNoDivergencePolicy)
	assert.Empty(t, policy, "a refused value yields no policy to pass on")
	_, err = ParseDivergencePolicy("fix")
	require.ErrorIs(t, err, ErrNoDivergencePolicy)
}

// An Outcome nobody minted carries no proof and is not clean: a consumer
// that reads Clean, or either flag, before the error cannot be handed the
// forgeable zero value as a clean pass.
func TestOutcomeZeroValueMintsNothing(t *testing.T) {
	var outcome Outcome
	assert.False(t, outcome.Clean(), "no pass read anything clean")
	_, minted := outcome.CleanWatermark()
	assert.False(t, minted)
	_, minted = outcome.VerifiedShadow()
	assert.False(t, minted)
}

// A clean pass through a partial watermark proves its prefix and nothing
// more; only the complete watermark earns the whole-shadow proof (CO-1).
func TestProveMintsTheVerifiedShadowOnlyWhenComplete(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	v := &Verifier{
		shadow: fakeShadow{schema: "app", source: "orders", shadow: "_pgsprite_orders_new", sourceOID: 1, shadowOID: 2, columns: []string{"id"}},
		opts:   Options{Clock: fixedClock(now)},
	}

	partial := v.prove(Report{Through: copier.NewWatermark(1500), Chunks: 2, Rows: 1500})
	clean, minted := partial.CleanWatermark()
	require.True(t, minted)
	assert.Equal(t, copier.NewWatermark(1500), clean.Watermark())
	_, minted = partial.VerifiedShadow()
	assert.False(t, minted, "keys above 1500 were not compared")

	complete := v.prove(Report{Through: copier.NewWatermark(math.MaxInt64), Chunks: 3, Rows: 2500})
	verified, minted := complete.VerifiedShadow()
	require.True(t, minted)
	assert.Equal(t, "app", verified.Schema())
	assert.Equal(t, "orders", verified.Table())
	assert.Equal(t, "_pgsprite_orders_new", verified.Shadow())
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), verified.Watermark())
	assert.Equal(t, now, verified.VerifiedAt())
}

// The two divergence errors name the chunks and counts an operator needs
// to see what differed without opening the report.
func TestDivergenceErrorMessages(t *testing.T) {
	c, err := copier.NewChunk(1001, 2000)
	require.NoError(t, err)
	mismatch := Mismatch{Chunk: c, Source: Digest{Rows: 1000, Hash: "a"}, Shadow: Digest{Rows: 999, Hash: "b"}}

	divergence := &DivergenceError{Report: Report{Through: copier.NewWatermark(2500), Chunks: 3, Mismatches: []Mismatch{mismatch}}}
	assert.EqualError(t, divergence, "source and shadow differ in 1 of 3 chunks through watermark 2500")

	repair := &RepairError{Repair: Repair{Mismatch: mismatch, Removed: 999, Inserted: 1000}, After: mismatch}
	assert.EqualError(t, repair, "chunk [1001, 2000] still differs after its repair: source 1000 rows, shadow 999 rows")
}

// fixedClock is a Clock that always reads the same instant.
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }
