package decode_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
)

// A slot nobody reads from retains every byte written after it: the
// retained WAL grows with writes and is measured from the slot's own
// restart position against the server's write position, the two values an
// operator would compare by hand.
func TestInspectSlotMeasuresTheWALTheSlotRetains(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	require.NoError(t, slot.Close(t.Context()))
	fresh, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, fresh.Active)
	assert.Zero(t, fresh.ActivePID)
	assert.NotZero(t, fresh.RestartLSN)

	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, repeat('x', 1000) FROM generate_series(1001, 2000) AS g`, f.schema))
	require.NoError(t, err)
	written := f.currentWALLSN(t)

	after, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, fresh.RestartLSN, after.RestartLSN, "nothing confirmed, so the slot's floor does not move")
	require.True(t, fresh.Retained.Known)
	require.True(t, after.Retained.Known)
	assert.Greater(t, after.Retained.Bytes, fresh.Retained.Bytes)
	assert.GreaterOrEqual(t, after.Retained.Bytes, int64(written-after.RestartLSN),
		"retained bytes span from the restart position to at least the write position the insert reached")
}

// A slot whose WAL the server has removed is reported with the server's
// own verdict, so slot loss (ST-4) is a value the engine reads, not a
// failure it discovers mid-stream. The cluster keeps one segment for slots;
// writing several and checkpointing past them loses the slot's WAL.
func TestInspectSlotReportsWALTheServerHasLost(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t,
		"wal_level=logical", "max_slot_wal_keep_size=16MB", "min_wal_size=32MB", "max_wal_size=32MB")
	f := newSlotFixtureOn(t, serverURL, testutil.NewDatabase(t, serverURL))
	slot := f.createSlot(t)
	require.NoError(t, slot.Close(t.Context()))

	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, repeat('x', 1000) FROM generate_series(1001, 80000) AS g`, f.schema))
	require.NoError(t, err)
	for _, sql := range []string{`SELECT pg_switch_wal()`, `CHECKPOINT`, `SELECT pg_switch_wal()`, `CHECKPOINT`} {
		_, err := f.pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}

	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, decode.WALStatusLost, status.WALStatus)
	assert.False(t, status.Retained.Known, "a slot with no restart position retains an unknown amount, not zero")
}

// A name no slot wears is not an error; it is the answer "no slot".
func TestInspectSlotReportsAnAbsentSlot(t *testing.T) {
	f := newSlotFixture(t)
	status, found, err := decode.InspectSlot(t.Context(), f.pool, f.target.DecodingName())
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, decode.SlotStatus{}, status)
}
