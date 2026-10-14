package checkpoint_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
)

// Inspect returns the target's row without being told which statement
// wrote it: the fingerprints come back on the Checkpoint for the caller to
// judge, as does the proof. A Load for another statement's fingerprints
// still refuses the same row, so Inspect widens only the read.
func TestInspectReturnsTheRowWhateverStatementWroteIt(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()
	cp.ShadowProof = ordersProof
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), cp))

	got, err := f.store.Inspect(t.Context(), "app", "orders")
	require.NoError(t, err)
	assert.Equal(t, cp.Fingerprints(), got.Fingerprints())
	assert.Equal(t, checkpoint.PhaseCopying, got.Phase)
	assert.JSONEq(t, string(ordersProof), string(got.ShadowProof))

	_, err = f.store.Load(t.Context(), "app", "orders", checkpoint.Fingerprints{Source: "src-a", Target: "tgt-other"})
	var incompatible *checkpoint.IncompatibleError
	assert.ErrorAs(t, err, &incompatible, "Load still guards on the statement")
}

// Inspect's other answers are Load's: no row is ErrNotFound, no table is
// ErrTableMissing, and a row in another format is the typed mismatch
// carrying the row's identity, since the proof of a row this engine cannot
// read is not one it may act on.
func TestInspectReportsNotFoundMissingTableAndAnotherFormat(t *testing.T) {
	f := newStoreFixture(t)
	_, err := f.store.Inspect(t.Context(), "app", "orders")
	assert.ErrorIs(t, err, checkpoint.ErrTableMissing)
	assert.NotErrorIs(t, err, checkpoint.ErrNotFound)

	f.ensure(t)
	_, err = f.store.Inspect(t.Context(), "app", "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound)

	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), ordersCheckpoint()))
	f.exec(t, "UPDATE pgsprite.pgsprite_checkpoint SET format_version = format_version + 1")
	_, err = f.store.Inspect(t.Context(), "app", "orders")
	var incompatible *checkpoint.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, checkpoint.MismatchFormat, incompatible.Mismatch)
	assert.Equal(t, checkpoint.FormatVersion+1, incompatible.Stored.FormatVersion)
	assert.Equal(t, ordersCheckpoint().Fingerprints(), incompatible.Stored.Fingerprints)
}
