package schemachange

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The shadow's size read names every catalog relation, function, and
// operator through pg_catalog, so a lookalike on the caller's search_path
// cannot change the figure the disk check discounts (CO-9).
func TestShadowBytesSQLQualifiesEveryName(t *testing.T) {
	testutil.AssertCatalogQualified(t, shadowBytesSQL)
}

// A zero shape names no table, so there is no stopped run to measure. It
// is refused before the checkpoint store or the server is read.
func TestMeasureResumeHeldRefusesAZeroShapeWithoutReadingAnything(t *testing.T) {
	held, err := MeasureResumeHeld(t.Context(), nil, nil, preflight.CopySwapShape{}, true)
	require.ErrorIs(t, err, preflight.ErrCopySwapProofMismatch)
	assert.Equal(t, preflight.CopySwapHeld{}, held)
}

// Only a reading that proves WAL the resumed run's own slot keeps on the
// volume is discounted: a missing slot, another database's slot, a lost
// slot the resume replaces, and a retained figure the server could not
// measure all count nothing, so the disk check falls back to the fresh
// requirement rather than discounting space it cannot see.
func TestCountsRetainedWAL(t *testing.T) {
	own := decode.SlotStatus{
		Own:       true,
		WALStatus: decode.WALStatusReserved,
		Retained:  decode.RetainedWAL{Bytes: 3 << 20, Known: true},
	}
	assert.True(t, countsRetainedWAL(own, true), "the route's own slot with measured WAL is counted")
	assert.False(t, countsRetainedWAL(own, false), "no slot of the name")

	foreign := own
	foreign.Own = false
	assert.False(t, countsRetainedWAL(foreign, true), "another database's or a physical slot")

	lost := own
	lost.WALStatus = decode.WALStatusLost
	assert.False(t, countsRetainedWAL(lost, true), "a lost slot is replaced by the resume")

	unknown := own
	unknown.Retained = decode.RetainedWAL{}
	assert.False(t, countsRetainedWAL(unknown, true), "an unmeasured figure is not read as held WAL")

	unknownWithBytes := own
	unknownWithBytes.Retained = decode.RetainedWAL{Bytes: 3 << 20, Known: false}
	assert.False(t, countsRetainedWAL(unknownWithBytes, true), "a figure the server marked unknown is not held WAL, whatever its bytes say")

	extended := own
	extended.WALStatus = decode.WALStatusExtended
	assert.True(t, countsRetainedWAL(extended, true), "a slot past max_wal_size still holds its WAL")
}

// Only a swap the catalog shows committed is reported as one: the source
// still live and a refusal — a swap still deciding, or relations the
// proof does not describe — leave the row measured as a resume that still
// copies, so the discount cannot reach a run that will copy again. A read
// that did not finish is returned.
func TestSwapOutcome(t *testing.T) {
	committed, err := swapOutcome(nil)
	require.NoError(t, err)
	assert.True(t, committed, "a minted swap proof")

	committed, err = swapOutcome(fmt.Errorf("wrapped: %w", ErrOldTableNotFound))
	require.NoError(t, err)
	assert.True(t, committed, "the retained source already dropped")

	committed, err = swapOutcome(fmt.Errorf("wrapped: %w", ErrNotSwapped))
	require.NoError(t, err)
	assert.False(t, committed, "the source still live")

	committed, err = swapOutcome(refuse(CauseOutcomeAmbiguous, nil, "a swap still deciding"))
	require.NoError(t, err)
	assert.False(t, committed, "a refusal proves nothing")

	failed := errors.New("connection reset")
	_, err = swapOutcome(failed)
	assert.ErrorIs(t, err, failed, "a read that did not finish is returned")
}
