package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transaction is visible when it completed before the snapshot: below
// xmin, or between xmin and xmax and not in the in-progress list. The
// in-progress list and xmax are judged exactly, on both sides of each
// boundary.
func TestSnapshotVisibleJudgesEachTransactionAgainstTheHorizon(t *testing.T) {
	snap, err := parseSnapshot("100:110:100,103,107")
	require.NoError(t, err)

	assert.True(t, snap.Visible(99), "below xmin: completed")
	assert.False(t, snap.Visible(100), "xmin itself is the oldest in progress")
	assert.True(t, snap.Visible(101), "between the horizons and not in progress")
	assert.False(t, snap.Visible(103), "in progress")
	assert.True(t, snap.Visible(109), "just below xmax, not in progress")
	assert.False(t, snap.Visible(110), "xmax: not yet marked complete")
	assert.False(t, snap.Visible(140), "above xmax: delivered by the stream before the server marked it complete")
}

// A snapshot with nothing in progress still hides transactions at or
// above xmax.
func TestSnapshotWithNothingInProgress(t *testing.T) {
	snap, err := parseSnapshot("110:110:")
	require.NoError(t, err)

	assert.True(t, snap.Visible(109))
	assert.False(t, snap.Visible(110))
}

// The stream's 32-bit transaction id is placed in the epoch nearest the
// snapshot's xmax: one just below xmax's low bits that wrapped is the
// previous epoch's and completed; one just above that wrapped is the next
// epoch's and not yet complete.
func TestSnapshotVisibleLiftsTransactionsAcrossAnEpochBoundary(t *testing.T) {
	const epoch = uint64(1) << 32

	intoNewEpoch, err := parseSnapshot("4294967290:4294967301:") // xmin 0xFFFFFFFA, xmax epoch 1 | 5
	require.NoError(t, err)
	assert.True(t, intoNewEpoch.Visible(0xFFFFFFF0), "previous epoch, below xmin")
	assert.True(t, intoNewEpoch.Visible(3), "this epoch, below xmax")
	assert.False(t, intoNewEpoch.Visible(5), "this epoch, xmax itself")

	beforeNewEpoch := Snapshot{read: true, xmin: epoch - 10, xmax: epoch - 5, inProgress: map[uint64]struct{}{}}
	assert.True(t, beforeNewEpoch.Visible(0xFFFFFFF0), "this epoch, below xmin")
	assert.False(t, beforeNewEpoch.Visible(7), "next epoch: a commit the server has not marked complete")
}

// The zero snapshot shows nothing visible, so a drain with no snapshot
// keeps every entry; AllVisible shows everything, for a drain under a lock
// that excludes every writer.
func TestSnapshotZeroHidesEverythingAndAllVisibleShowsEverything(t *testing.T) {
	assert.False(t, Snapshot{}.Visible(1))
	assert.False(t, Snapshot{}.Visible(0xFFFFFFFF))
	assert.True(t, AllVisible().Visible(1))
	assert.True(t, AllVisible().Visible(0xFFFFFFFF))
}

// A snapshot the server rendered in an unexpected shape is an invariant
// violation, not a guess at what it meant.
func TestParseSnapshotRefusesAMalformedSnapshot(t *testing.T) {
	for _, text := range []string{"", "100", "100:110", "a:110:", "100:b:", "100:110:c", "120:110:"} {
		_, err := parseSnapshot(text)
		assert.ErrorIs(t, err, ErrInvariantViolation, text)
	}
}
