package schemachange_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// Nothing is true only when no leftover of any kind is named: the table's
// names alone are nothing, and each kind of leftover on its own is
// something.
func TestDiscardedNothingCountsEveryKindOfLeftover(t *testing.T) {
	names := schemachange.Discarded{Schema: "app", Table: "orders"}
	assert.True(t, names.Nothing())

	for kind, discarded := range map[string]schemachange.Discarded{
		"row phase":   {Schema: "app", Table: "orders", RowPhase: checkpoint.PhaseCopying},
		"shadow":      {Schema: "app", Table: "orders", Shadow: "_pgsprite_0123456789abcdef_new"},
		"slot":        {Schema: "app", Table: "orders", Slot: "pgsprite_01234567"},
		"publication": {Schema: "app", Table: "orders", Publication: "pgsprite_01234567"},
		"write fence": {Schema: "app", Table: "orders", WriteFence: true},
	} {
		assert.False(t, discarded.Nothing(), kind)
	}
}

// A table lock release that fails after a discard that completed is
// ErrLockReleaseFailed wrapping the release's error, so a caller can tell
// a complete discard whose lock release failed from a partial one. A
// release that fails after a discard that itself failed is joined with
// the discard's error without the sentinel: that discard is partial, and
// the sentinel must not say otherwise. A release that succeeds leaves the
// discard's outcome as it was.
func TestDiscardLockReleaseFailureIsTypedOnlyAfterACompleteDiscard(t *testing.T) {
	release := errors.New("unlock: connection reset")
	partial := errors.New("drop shadow: lock timeout")

	completed := schemachange.JoinDiscardRelease(nil, "app", "orders", release)
	require.Error(t, completed)
	assert.ErrorIs(t, completed, schemachange.ErrLockReleaseFailed)
	assert.ErrorIs(t, completed, release, "the release's own error stays in the chain")

	failed := schemachange.JoinDiscardRelease(partial, "app", "orders", release)
	require.Error(t, failed)
	assert.NotErrorIs(t, failed, schemachange.ErrLockReleaseFailed, "a partial discard never reads as complete")
	assert.ErrorIs(t, failed, partial)
	assert.ErrorIs(t, failed, release)

	assert.NoError(t, schemachange.JoinDiscardRelease(nil, "app", "orders", nil))
	assert.Same(t, partial, schemachange.JoinDiscardRelease(partial, "app", "orders", nil))
}
