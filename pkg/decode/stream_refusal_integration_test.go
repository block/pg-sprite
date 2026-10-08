package decode_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// A stream is refused for a quiesced target, whose environment was never
// checked for decoding, and on a replication connection to another
// database, which would read another database's slot of the same name
// (ST-3); neither refusal touches the slot.
func TestOpenStreamRefusesAQuiescedTargetAndAnotherDatabase(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)

	quiesced := f.mintTarget(t, f.pool, false)
	_, err := decode.OpenStream(t.Context(), f.cfg, quiesced, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)

	other := dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)}
	_, err = decode.OpenStream(t.Context(), other, f.target, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
}

// A source renamed under the stream is no longer the target: its next
// change stops the stream rather than arriving under the old name.
func TestStreamStopsWhenTheSourceIsRenamed(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `ALTER TABLE %s.ledger RENAME TO ledger_moved`)
	f.exec(t, `INSERT INTO %s.ledger_moved (id, note) VALUES (101, 'renamed')`)

	require.ErrorIs(t, nextError(t, stream), decode.ErrInvariantViolation)
}

// A source whose key is no longer part of the replica identity cannot
// carry its key in an old tuple, so the stream stops at its description.
func TestStreamStopsWhenTheKeyLeavesTheReplicaIdentity(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.exec(t, `ALTER TABLE %s.ledger REPLICA IDENTITY NOTHING`)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'no identity')`)

	require.ErrorIs(t, nextError(t, stream), decode.ErrInvariantViolation)
}

// A publication dropped under the stream fails the decoder on the server;
// the stream returns the server's error, with its SQLSTATE reachable,
// rather than a protocol violation.
func TestStreamReturnsTheDecodersError(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())
	_, err := f.pool.Exec(t.Context(), `DROP PUBLICATION `+slot.Name())
	require.NoError(t, err)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'unpublished')`)

	requireSQLState(t, nextError(t, stream), "42704")
}

// The caller's own deadline ends the wait even when it falls inside it:
// only the wait running out is a quiet table, so a caller whose deadline
// has passed sees its error rather than progress deliveries without end.
func TestNextReturnsTheCallersDeadline(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	const callerDeadline = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), callerDeadline)
	defer cancel()
	until := time.Now().Add(streamDeadline)
	for time.Now().Before(until) {
		if _, err := stream.Next(ctx, streamDeadline); err != nil {
			require.ErrorIs(t, err, context.DeadlineExceeded)
			return
		}
	}
	require.FailNow(t, "Next kept yielding progress after the caller's deadline")
}
