package checkpoint_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// A store over a caller's pool whose search_path lists a type named text
// ahead of pg_catalog, one that keeps only three characters, reads back the
// applied position the run saved. Load casts the position through
// pg_catalog.text, so the lookalike never reaches it (CO-9). Were the cast
// bare, the position would come back as 0/1, which decode.ParseLSN accepts,
// and a resume would open its stream from a position the run never saved.
func TestLoadReadsTheSavedPositionUnderALookalikeTextType(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), cp))

	lookalike := testutil.NewLookalikeTextTypePool(t, f.url, f.pool)
	store, err := checkpoint.NewStore(lookalike, checkpoint.Options{Clock: f.clock})
	require.NoError(t, err)

	got, err := store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	require.NoError(t, err)
	assert.Equal(t, cp.LastAppliedLSN, got.LastAppliedLSN)
}

// A store over a caller's pool whose search_path lists an = and a <> on
// text that answer false for every pair finds the target's row. Load
// spells its comparisons OPERATOR(pg_catalog.=), so the lookalikes never
// reach it (CO-9). Were they bare, Load would report the row missing and a
// resume would start the run over. Save's guard and Delete are held to the
// same spelling by TestRowStatementsQualifyEveryCastAndOperator; they are
// not driven here because both first confirm the table lock, whose
// pg_locks read is not yet qualified and refuses on this pool.
func TestLoadFindsTheRowUnderLookalikeTextComparisons(t *testing.T) {
	f := newStoreFixture(t)
	f.ensure(t)
	cp := ordersCheckpoint()
	require.NoError(t, f.store.Save(t.Context(), f.ordersLock(t), cp))

	lookalike := testutil.NewLookalikeTextMismatchPool(t, f.url, f.pool)
	store, err := checkpoint.NewStore(lookalike, checkpoint.Options{Clock: f.clock})
	require.NoError(t, err)

	got, err := store.Load(t.Context(), "app", "orders", cp.Fingerprints())
	require.NoError(t, err, "the row is found")
	assert.Equal(t, cp.Watermark, got.Watermark)
}
