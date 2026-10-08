package decode_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
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
	_, err := decode.OpenStream(t.Context(), f.cfg, f.pool, quiesced, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)

	other := dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)}
	_, err = decode.OpenStream(t.Context(), other, f.pool, f.target, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
}

// A database name is not a server: the same table on two clusters derives
// the same slot name, so a replication connection to a database of the
// target's name on another cluster is refused before START_REPLICATION,
// rather than decoding that cluster's slot as this target's changes (ST-3).
// The refusal is the stream's own, not the other server's complaint that
// the slot does not exist there.
func TestOpenStreamRefusesAReplicationConnectionOnAnotherServer(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	elsewhere := sameNamedDatabaseElsewhere(t, f)

	stream, err := decode.OpenStream(t.Context(), elsewhere.cfg, f.pool, f.target, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, stream)
}

// The cluster proof ties the replication connection to the pool, not to the
// target: a connection and a pool that agree on another database of the
// same cluster are refused as the stream's own ST-3 violation, before the
// server is asked for a slot that database does not hold.
func TestOpenStreamRefusesAPoolOnAnotherDatabase(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)

	other := dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)}
	otherPool, err := dbconn.NewPool(t.Context(), other)
	require.NoError(t, err)
	t.Cleanup(otherPool.Close)

	stream, err := decode.OpenStream(t.Context(), other, otherPool, f.target, slot.ConsistentPoint())
	require.ErrorIs(t, err, decode.ErrInvariantViolation)
	assert.Nil(t, stream)
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

// A publication dropped under the stream ends it with the server's
// SQLSTATE reachable: before PostgreSQL 18 the decoder fails on the missing
// publication (42704); from 18 it skips loading the publication with a
// warning (55000) and sends nothing for the change, which the stream
// refuses as an ST-4 violation rather than letting a keepalive step past
// the change the slot will never resend.
func TestStreamReturnsTheDecodersError(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.dropPublicationUnder(t, slot)
	f.requireDroppedPublicationStops(t, stream)
}

// A database that sends its sessions only errors cannot hide a withheld
// change from the stream: the replication connection asks for warnings
// itself, so a publication dropped under the stream still ends it with the
// server's SQLSTATE on every major.
func TestStreamStopsOnTheWarningWhenTheDatabaseSendsOnlyErrors(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	var database string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database))
	_, err := f.pool.Exec(t.Context(), `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` SET client_min_messages = error`)
	require.NoError(t, err)
	stream := f.openStream(t, slot.ConsistentPoint())

	f.dropPublicationUnder(t, slot)
	f.requireDroppedPublicationStops(t, stream)
}

// dropPublicationUnder drops the slot's publication under an open stream and
// commits one change the walsender can no longer send.
func (f slotFixture) dropPublicationUnder(t *testing.T, slot *decode.Slot) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), `DROP PUBLICATION `+slot.Name())
	require.NoError(t, err)
	f.exec(t, `INSERT INTO %s.ledger (id, note) VALUES (101, 'unpublished')`)
}

// requireDroppedPublicationStops asserts the stream ends with the server's
// SQLSTATE for a dropped publication: the decoder's missing-publication
// error before PostgreSQL 18, the stream's own ST-4 refusal of the
// skipped-publication warning from 18.
func (f slotFixture) requireDroppedPublicationStops(t *testing.T, stream *decode.Stream) {
	t.Helper()
	var version int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	const skipsAMissingPublicationSince = 180000
	err := nextError(t, stream)
	if version >= skipsAMissingPublicationSince {
		require.ErrorIs(t, err, decode.ErrInvariantViolation)
		requireSQLState(t, err, "55000")
		return
	}
	requireSQLState(t, err, "42704")
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
