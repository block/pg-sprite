package decode_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
)

// walSenderTimeout is short enough that the server sends keepalives asking
// for a reply several times within one test; the stream must answer them
// without moving the slot.
const walSenderTimeout = "wal_sender_timeout=2s"

// newKeepaliveFixture is a fixture on a server that keeps the stream busy
// answering keepalives.
func newKeepaliveFixture(t *testing.T) slotFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", walSenderTimeout)
	return newSlotFixtureOn(t, serverURL, testutil.NewDatabase(t, serverURL))
}

// Confirming a delivered position is what moves the slot: the server's
// recorded position becomes exactly it.
func TestConfirmMovesTheSlotToTheConfirmedPosition(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	assert.Equal(t, decode.LSN(0), stream.Confirmed(), "nothing is confirmed until the caller says so")

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'to confirm')`)
	ev := nextChange(t, stream)
	awaitDelivered(t, stream, ev.LSN+1)
	delivered := stream.Delivered()

	require.NoError(t, stream.Confirm(t.Context(), delivered))
	assert.Equal(t, delivered, stream.Confirmed())
	f.assertConfirmedFlushBecomes(t, slot, delivered)
}

// The stream answers the server's keepalives — the connection survives
// several wal_sender_timeout periods — but a reply carries only what the
// caller confirmed, so a quiet stream that confirms nothing leaves the slot
// where it was, however far the server's own position runs ahead.
func TestKeepaliveRepliesDoNotMoveTheSlot(t *testing.T) {
	f := newKeepaliveFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	initial := f.confirmedFlush(t, slot)

	// Writes to a table the publication does not cover move the server's
	// position without producing a change.
	f.exec(t, `CREATE TABLE %s.other (id bigint PRIMARY KEY)`)
	f.exec(t, `INSERT INTO %s.other SELECT g FROM generate_series(1, 1000) AS g`)
	serverPosition := f.currentWALLSN(t)

	const severalTimeouts = 5 * time.Second
	until := time.Now().Add(severalTimeouts)
	for time.Now().Before(until) {
		d, err := stream.Next(t.Context(), streamWait)
		require.NoError(t, err, "the stream must outlive wal_sender_timeout by answering keepalives")
		assert.Nil(t, d.Change)
	}

	assert.GreaterOrEqual(t, stream.Delivered(), serverPosition, "between transactions the keepalive position is delivered")
	assert.Equal(t, decode.LSN(0), stream.Confirmed())
	assert.Equal(t, initial, f.confirmedFlush(t, slot), "keepalive replies carry no position")

	require.NoError(t, stream.Confirm(t.Context(), stream.Delivered()))
	f.assertConfirmedFlushBecomes(t, slot, stream.Confirmed())
}

// A position beyond what the stream has delivered is refused, since
// confirming it would let the server drop changes the caller never saw,
// and the slot is left where it was.
func TestConfirmRefusesAPositionBeyondDelivered(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	initial := f.confirmedFlush(t, slot)

	err := stream.Confirm(t.Context(), stream.Delivered()+1)
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Equal(t, decode.LSN(0), stream.Confirmed())
	assert.Equal(t, initial, f.confirmedFlush(t, slot))
}

// A stream reopened from a confirmed position sees again every transaction
// that committed above it and none that committed at or below it; a stream
// reopened from an older position is forwarded by the server to the
// confirmed one, so a stale checkpoint replays no more than the slot
// retains. Confirming below the slot's position is accepted and leaves the
// slot where it was: a replayed transaction's early changes lie below the
// resume point.
func TestReopenedStreamReplaysFromTheConfirmedPosition(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'first')`)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (102, 'second')`)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (103, 'third')`)

	first := nextChange(t, stream)
	require.EqualValues(t, 101, first.Key)
	second := nextChange(t, stream)
	require.EqualValues(t, 102, second.Key)
	awaitDelivered(t, stream, second.LSN+1)
	afterSecond := stream.Delivered()
	third := nextChange(t, stream)
	require.EqualValues(t, 103, third.Key)
	require.GreaterOrEqual(t, third.LSN, afterSecond, "the third transaction starts where the second ended")

	require.NoError(t, stream.Confirm(t.Context(), afterSecond))
	f.assertConfirmedFlushBecomes(t, slot, afterSecond)
	require.NoError(t, stream.Close(t.Context()))

	resumed := f.openStream(t, afterSecond)
	assert.Equal(t, afterSecond, resumed.Start())
	assert.Equal(t, afterSecond, resumed.Delivered(), "nothing is delivered before the start")
	replayed := nextChange(t, resumed)
	assert.EqualValues(t, 103, replayed.Key, "the third transaction is replayed, the first two are not")
	assert.Equal(t, third, replayed, "a replayed change is the same change")

	require.NoError(t, resumed.Confirm(t.Context(), slot.ConsistentPoint()))
	assert.Equal(t, slot.ConsistentPoint(), resumed.Confirmed())
	assert.Equal(t, afterSecond, f.confirmedFlush(t, slot), "the server never moves the slot backwards")
	require.NoError(t, resumed.Close(t.Context()))

	stale := f.openStream(t, slot.ConsistentPoint())
	forwarded := nextChange(t, stale)
	assert.EqualValues(t, 103, forwarded.Key, "the server forwards a start below the slot's position")
}
