package checkpoint_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// A run on orders is mid copy and one on ledger is done. Only the copying
// row is unfinished: it is listed with its slot and the time its save
// stamped, and the done row is not, by the same terminal rule the reaper
// keeps a slot by.
func TestUnfinishedListsOnlyRowsInANonTerminalPhase(t *testing.T) {
	d := newReapDatabase(t, testutil.StartPostgres(t))
	require.NoError(t, d.store.Ensure(t.Context()))
	d.claim(t, "orders", "pgsprite_0123abcd", checkpoint.PhaseCopying)
	d.claim(t, "ledger", "pgsprite_4567cdef", checkpoint.PhaseDone)
	saved, err := d.store.Inspect(t.Context(), d.schema, "orders")
	require.NoError(t, err)

	unfinished, err := d.store.Unfinished(t.Context())

	require.NoError(t, err)
	require.Len(t, unfinished, 1)
	assert.Equal(t, checkpoint.Unfinished{
		Schema:    d.schema,
		Table:     "orders",
		Phase:     "copying",
		SlotName:  "pgsprite_0123abcd",
		UpdatedAt: saved.UpdatedAt,
	}, unfinished[0])
}

// A database where no run ever ran has no checkpoint table, which is
// ErrTableMissing rather than an empty list a reader could not tell from
// one with nothing in flight.
func TestUnfinishedWithoutTheCheckpointTableIsErrTableMissing(t *testing.T) {
	d := newReapDatabase(t, testutil.StartPostgres(t))

	_, err := d.store.Unfinished(t.Context())

	assert.ErrorIs(t, err, checkpoint.ErrTableMissing)
}
