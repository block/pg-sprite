package decode_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// awayFromNext is how long the caller stays away from Next: several times
// the keepalive fixture's wal_sender_timeout, so a stream nothing answers
// for is ended by the server well within it.
const awayFromNext = 6 * time.Second

// slotActive reports whether a walsender holds the slot.
func (f slotFixture) slotActive(t *testing.T, slot *decode.Slot) bool {
	t.Helper()
	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	return status.Active
}

// A caller that stays away from Next for longer than wal_sender_timeout —
// in a flush, or waiting on a pool — keeps its stream: the heartbeat
// reports to the server meanwhile, so the walsender holds the slot
// throughout, and the next Next delivers the change committed while the
// caller was away. The heartbeat reports only what the caller confirmed:
// the stream has delivered a second change past the confirmed position,
// and after the silence both the walsender's record of the client's flush
// position and the slot's confirmed position are still the confirmed one.
func TestHeartbeatKeepsAStreamTheCallerIsAwayFromWithoutMovingTheSlot(t *testing.T) {
	f := newKeepaliveFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'confirmed')`)
	first := nextChange(t, stream)
	awaitDelivered(t, stream, first.LSN+1)
	require.NoError(t, stream.Confirm(t.Context(), stream.Delivered()))
	confirmed := stream.Confirmed()
	stream.assertConfirmedFlushBecomes(t, slot, confirmed)
	stream.assertWALSenderFlushBecomes(t, slot, confirmed)
	confirmReply := f.slotWALSenderReply(t, slot)

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (102, 'delivered, not confirmed')`)
	second := nextChange(t, stream)
	awaitDelivered(t, stream, second.LSN+1)
	require.Greater(t, stream.Delivered(), confirmed, "the stream holds a delivered position past the confirmed one")

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (103, 'committed while the caller was away')`)
	for until := time.Now().Add(awayFromNext); time.Now().Before(until); time.Sleep(50 * time.Millisecond) {
		require.True(t, f.slotActive(t, slot), "the server ended the stream while the caller was away from Next")
	}

	reply := f.slotWALSenderReply(t, slot)
	assert.True(t, reply.at.After(confirmReply.at), "the walsender heard from the stream while the caller was away")
	require.NotNil(t, reply.flush)
	assert.Equal(t, confirmed, *reply.flush, "the heartbeat reports the confirmed position, not the delivered one")
	assert.Equal(t, confirmed, f.confirmedFlush(t, slot), "the slot stays where the caller's Confirm put it")

	third := nextChange(t, stream)
	assert.EqualValues(t, 103, third.Key)
	assert.Equal(t, confirmed, stream.Confirmed())
}
