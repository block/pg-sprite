package decode_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// The drop's ownership check judges the slot's database with
// OPERATOR(pg_catalog.=), so a caller's pool whose search_path lists a
// lookalike = on name that answers true for everything still sees another
// database's slot of the name as foreign: the drop is refused and the
// slot stays (CO-9, ST-3). A bare = would call the slot this database's
// and drop it.
func TestDropIdleSlotRefusesAnotherDatabasesSlotUnderALookalikeEquals(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)})
	require.NoError(t, err)
	t.Cleanup(other.Close)
	_, err = other.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := other.Exec(context.WithoutCancel(t.Context()), `
			SELECT pg_drop_replication_slot(slot_name)
			FROM pg_replication_slots
			WHERE slot_name = $1`, name)
		assert.NoError(t, err)
	})
	lookalike := testutil.NewLookalikeNameMatchPool(t, f.databaseURL, f.pool)

	err = decode.DropIdleSlot(t.Context(), f.cfg, lookalike, name)

	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	var foreign *decode.ForeignStateError
	require.ErrorAs(t, err, &foreign)
	assert.Equal(t, decode.ForeignObjectSlot, foreign.Object)
	status, found, err := decode.InspectSlot(t.Context(), other, name)
	require.NoError(t, err)
	assert.True(t, found, "another database's slot is left alone")
	assert.True(t, status.Own, "the slot is its own database's")
}
