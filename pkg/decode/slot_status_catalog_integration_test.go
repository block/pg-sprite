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

// The slot inspection judges a slot's database with OPERATOR(pg_catalog.=),
// so a caller's pool whose search_path lists a lookalike = on name that
// answers true for everything still reads another database's slot of the
// name as foreign (CO-9, ST-3). A bare = would call it this database's own.
func TestInspectSlotReadsAnotherDatabasesSlotAsForeignUnderALookalikeEquals(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	otherDatabase := createSlotInAnotherDatabase(t, f, name)
	lookalike := testutil.NewLookalikeNameMatchPool(t, f.databaseURL, f.pool)

	status, found, err := decode.InspectSlot(t.Context(), lookalike, name)

	require.NoError(t, err)
	require.True(t, found, "the slot of the name exists on the cluster")
	assert.False(t, status.Own, "another database's slot is never this database's")
	assert.True(t, status.Logical)
	assert.Equal(t, otherDatabase, status.Database)
}

// The slot inspection finds the slot by its name with
// OPERATOR(pg_catalog.=), so under a lookalike = on name that answers true
// for everything a name no slot holds still reads as not found, though the
// cluster has another slot. A bare = would match that other slot and report
// it under the missing name, which the catch-up's lag watch would take for a
// live slot.
func TestInspectSlotDoesNotFindAMissingSlotUnderALookalikeEquals(t *testing.T) {
	f := newSlotFixture(t)
	createSlotInAnotherDatabase(t, f, f.target.DecodingName())
	lookalike := testutil.NewLookalikeNameMatchPool(t, f.databaseURL, f.pool)

	status, found, err := decode.InspectSlot(t.Context(), lookalike, "pgsprite_no_such_slot")

	require.NoError(t, err)
	assert.False(t, found, "no slot holds the name")
	assert.Equal(t, decode.SlotStatus{}, status)
}

// createSlotInAnotherDatabase creates a logical slot of name in a fresh
// database on the fixture's server, drops it at cleanup, and returns that
// database's name.
func createSlotInAnotherDatabase(t *testing.T, f slotFixture, name string) string {
	t.Helper()
	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)})
	require.NoError(t, err)
	t.Cleanup(other.Close)
	var database string
	require.NoError(t, other.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database))
	_, err = other.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := other.Exec(context.WithoutCancel(t.Context()), `
			SELECT pg_drop_replication_slot(slot_name)
			FROM pg_replication_slots
			WHERE slot_name = $1`, name)
		assert.NoError(t, err)
	})
	return database
}
