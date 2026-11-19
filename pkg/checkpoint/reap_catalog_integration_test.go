package checkpoint_test

import (
	"fmt"
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

// A run in copying owns its slot, and the reaper leaves it (ST-3). The
// live-row read spells its comparisons OPERATOR(pg_catalog.…), so a
// caller's pool whose search_path lists an = and a <> on text that answer
// false for everything still reads the row as live and keeps the slot
// (CO-9). Were either comparison bare, the row would read as terminal or
// absent and the reaper would drop a running schema change's slot.
func TestReaperKeepsALiveRowsSlotUnderLookalikeTextComparisons(t *testing.T) {
	d := newReapDatabase(t, newReapServer(t))
	require.NoError(t, d.store.Ensure(t.Context()))
	_, live := d.slotFor(t, "orders")
	d.claim(t, "orders", live.Name(), checkpoint.PhaseCopying)

	schema := testutil.NewSchema(t, d.pool)
	for _, ddl := range []string{
		`
		CREATE FUNCTION %[1]s.never_text(text, text) RETURNS boolean
		LANGUAGE sql AS 'SELECT false'`,
		`
		CREATE OPERATOR %[1]s.= (
			LEFTARG  = text,
			RIGHTARG = text,
			FUNCTION = %[1]s.never_text
		)`,
		`
		CREATE OPERATOR %[1]s.<> (
			LEFTARG  = text,
			RIGHTARG = text,
			FUNCTION = %[1]s.never_text
		)`,
	} {
		_, err := d.pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, "plant a lookalike text operator")
	}
	lookalike := testutil.NewCatalogShadowingPool(t, d.cfg.URL, schema)
	var equal, unequal bool
	require.NoError(t, lookalike.QueryRow(t.Context(),
		`SELECT 'a'::text = 'a'::text, 'a'::text <> 'b'::text`).Scan(&equal, &unequal))
	require.False(t, equal, "the lookalike text = must answer on this pool")
	require.False(t, unequal, "the lookalike text <> must answer on this pool")
	store, err := checkpoint.NewStore(lookalike, checkpoint.Options{})
	require.NoError(t, err)

	reaped, err := store.ReapOrphanSlots(t.Context(), d.cfg)

	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{Live: []string{live.Name()}}, reaped)
	assert.True(t, slotExists(t, d.pool, live.Name()), "a live run's slot is not the reaper's to drop")
}
