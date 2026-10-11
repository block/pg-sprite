package decode_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
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
	stream.assertConfirmedFlushBecomes(t, slot, delivered)
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
	stream.assertConfirmedFlushBecomes(t, slot, stream.Confirmed())
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
	stream.assertConfirmedFlushBecomes(t, slot, afterSecond)
	require.NoError(t, stream.Close(t.Context()))

	resumed := f.openStream(t, afterSecond)
	assert.Equal(t, afterSecond, resumed.Start())
	assert.Equal(t, afterSecond, resumed.Delivered(), "nothing is delivered before the start")
	replayed := nextChange(t, resumed)
	assert.EqualValues(t, 103, replayed.Key, "the third transaction is replayed, the first two are not")
	assertSameChange(t, third, replayed)

	require.NoError(t, resumed.Confirm(t.Context(), slot.ConsistentPoint()))
	assert.Equal(t, slot.ConsistentPoint(), resumed.Confirmed())
	resumed.assertWALSenderFlushBecomes(t, slot, slot.ConsistentPoint())
	assert.Equal(t, afterSecond, f.confirmedFlush(t, slot), "the server never moves the slot backwards")
	require.NoError(t, resumed.Close(t.Context()))

	stale := f.openStream(t, slot.ConsistentPoint())
	forwarded := nextChange(t, stale)
	assert.EqualValues(t, 103, forwarded.Key, "the server forwards a start below the slot's position")
}

// A transaction that writes the target before another transaction commits,
// and commits after it, arrives after that commit with a change position
// below what the stream already delivered and the caller confirmed. The
// slot replays by commit position, so a stream reopened from the
// confirmation sees the change again; the position a caller may confirm
// while that change is unapplied is the Delivered it arrived with — which
// is what the applier's buffer keys on — never the change's own LSN.
func TestStreamDeliversAnInterleavedTransactionBelowTheConfirmedPosition(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	open, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Rolling back a transaction the test already committed is a no-op.
		if err := open.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back the open transaction: %v", err)
		}
	})
	_, err = open.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.ledger (id, note) VALUES (201, 'written first')`, f.schema))
	require.NoError(t, err)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (202, 'committed first')`)
	first := nextChange(t, stream)
	require.EqualValues(t, 202, first.Key)
	awaitDelivered(t, stream, first.LSN+1)
	confirmed := stream.Delivered()
	require.NoError(t, stream.Confirm(t.Context(), confirmed))
	stream.assertConfirmedFlushBecomes(t, slot, confirmed)

	require.NoError(t, open.Commit(t.Context()))
	late := nextChangeDelivery(t, stream)
	require.EqualValues(t, 201, late.Change.Key)
	assert.Less(t, late.Change.LSN, confirmed, "the change was written below the confirmed position")
	assert.GreaterOrEqual(t, late.Delivered, confirmed)
	assert.Equal(t, late.Delivered, late.Change.Delivered, "a change carries the position it arrived with")

	buf := applier.NewBuffer()
	require.NoError(t, buf.Add(*late.Change))
	oldest, pending := buf.OldestPending()
	require.True(t, pending)
	assert.Equal(t, late.Delivered, oldest, "the buffer holds the stream to the position the change arrived with")
	require.NoError(t, stream.Confirm(t.Context(), min(stream.Delivered(), oldest)), "the buffer's bound is confirmable")
	require.NoError(t, stream.Close(t.Context()))

	replayed := nextChange(t, f.openStream(t, confirmed))
	assertSameChange(t, *late.Change, replayed)
}

// A transaction that wrote before another committed arrives after that
// commit. Its change carries the WAL position it was written at, which lies
// below what the stream already delivered, so the server position the
// stream reports for lag must not follow it there: the lag read from it
// would otherwise wrap below zero.
func TestServerWALEndNeverTrailsDelivered(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	open, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Rolling back a transaction the test already committed is a no-op.
		if err := open.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back the open transaction: %v", err)
		}
	})
	_, err = open.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.ledger (id, note) VALUES (201, 'written first')`, f.schema))
	require.NoError(t, err)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (202, 'committed first')`)
	first := nextChange(t, stream)
	require.EqualValues(t, 202, first.Key)
	awaitDelivered(t, stream, first.LSN+1)
	assert.GreaterOrEqual(t, stream.ServerWALEnd(), stream.Delivered(), "after a commit")

	require.NoError(t, open.Commit(t.Context()))
	late := nextChangeDelivery(t, stream)
	require.EqualValues(t, 201, late.Change.Key)
	require.Less(t, late.Change.LSN, stream.Delivered(), "the change was written below the delivered position")
	assert.GreaterOrEqual(t, stream.ServerWALEnd(), stream.Delivered(), "after a change written below Delivered")
}

// A stopped stream refuses to confirm with the error that stopped it: the
// caller's position is no longer one the stream can vouch for, and the
// record of what was confirmed is unchanged.
func TestConfirmRefusesAfterTheStreamStopped(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	f.exec(t, `TRUNCATE %s.ledger`)
	require.ErrorIs(t, nextError(t, stream), decode.ErrUnsupportedChange)

	err := stream.Confirm(t.Context(), stream.Delivered())

	require.ErrorIs(t, err, decode.ErrUnsupportedChange)
	assert.Equal(t, decode.LSN(0), stream.Confirmed())
}

// A confirmation the server could not be told is not recorded: the
// connection is gone, so the report never left, and Confirmed stays where
// it was rather than claim a position the server never heard. The stream
// is stopped from then on.
func TestConfirmLeavesTheRecordWhenTheSendFails(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'never confirmed')`)
	ev := nextChange(t, stream)
	awaitDelivered(t, stream, ev.LSN+1)
	initial := f.confirmedFlush(t, slot)
	require.NoError(t, stream.Close(t.Context()))

	err := stream.Confirm(t.Context(), stream.Delivered())

	require.Error(t, err)
	assert.Equal(t, decode.LSN(0), stream.Confirmed())
	_, nextErr := stream.Next(t.Context(), streamWait)
	assert.ErrorIs(t, nextErr, err, "the failed send stops the stream")
	assert.Equal(t, initial, f.confirmedFlush(t, slot), "the slot is where it was")
}
