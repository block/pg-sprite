package checkpoint_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
)

// Two databases share one cluster that decodes WAL, and the other
// database holds an orphan slot of the engine's shape. The reaper runs
// for this database over a caller's pool whose search_path lists, ahead
// of pg_catalog, an = on name and a LIKE on name that answer true for
// everything. The slot reads and the drop-side ownership check spell
// their comparisons OPERATOR(pg_catalog.…), so the other database's slot
// is never listed as this database's, nothing is reaped, and the slot
// stays (CO-9, ST-3, D11). Were the slot's database compared with a bare
// =, the lookalike would list it, the drop's ownership check would agree,
// and another run's slot would be dropped under it.
func TestReaperLeavesAnotherDatabasesSlotUnderALookalikeEquals(t *testing.T) {
	serverURL := newReapServer(t)
	d := newReapDatabase(t, serverURL)
	require.NoError(t, d.store.Ensure(t.Context()))
	other := newReapDatabase(t, serverURL)
	_, elsewhere := other.slotFor(t, "orders")

	lookalike := testutil.NewLookalikeNameMatchPool(t, d.cfg.URL, d.pool)
	store, err := checkpoint.NewStore(lookalike, checkpoint.Options{})
	require.NoError(t, err)

	reaped, err := store.ReapOrphanSlots(t.Context(), d.cfg)

	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{}, reaped, "this database has no engine slots to judge")
	assert.True(t, slotExists(t, other.pool, elsewhere.Name()), "another database's slot is not this database's to drop")
}
