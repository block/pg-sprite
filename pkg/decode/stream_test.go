package decode

import (
	"encoding/binary"
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walData wraps a pgoutput message as the XLogData the server carries it in.
func walData(walStart LSN, message []byte) pglogrepl.XLogData {
	return pglogrepl.XLogData{WALStart: pglogrepl.LSN(walStart), ServerWALEnd: pglogrepl.LSN(walStart), WALData: message}
}

// beginMessage encodes a pgoutput BEGIN: final LSN, commit time, xid.
func beginMessage(finalLSN LSN) []byte {
	b := []byte{byte(pglogrepl.MessageTypeBegin)}
	b = binary.BigEndian.AppendUint64(b, uint64(finalLSN))
	b = binary.BigEndian.AppendUint64(b, 0)
	return binary.BigEndian.AppendUint32(b, 1)
}

// commitMessage encodes a pgoutput COMMIT: flags, commit LSN, end LSN,
// commit time.
func commitMessage(commitLSN, endLSN LSN) []byte {
	b := []byte{byte(pglogrepl.MessageTypeCommit), 0}
	b = binary.BigEndian.AppendUint64(b, uint64(commitLSN))
	b = binary.BigEndian.AppendUint64(b, uint64(endLSN))
	return binary.BigEndian.AppendUint64(b, 0)
}

// insertMessage encodes a pgoutput INSERT of a one-column row whose only
// column is the text key.
func insertMessage(relationID uint32, key string) []byte {
	b := []byte{byte(pglogrepl.MessageTypeInsert)}
	b = binary.BigEndian.AppendUint32(b, relationID)
	b = append(b, 'N')
	b = binary.BigEndian.AppendUint16(b, 1)
	b = append(b, pglogrepl.TupleDataTypeText)
	b = binary.BigEndian.AppendUint32(b, uint32(len(key)))
	return append(b, key...)
}

// keyOnlyRelation is the stream's record of a one-column table keyed on id.
func keyOnlyRelation(id uint32) *relation {
	return &relation{id: id, columns: []pglogrepl.RelationMessageColumn{{Flags: keyFlag, Name: "id"}}, keyIndex: 0}
}

// A keepalive's position is the server's send position, which can lie past
// records of a transaction that has not committed and so has not been
// yielded; it is delivered only between transactions, when every
// transaction sent so far committed below it. A keepalive that asks for no
// reply touches the connection not at all.
func TestKeepalivePositionIsDeliveredOnlyBetweenTransactions(t *testing.T) {
	s := &Stream{delivered: 100}

	d, yielded, err := s.handleKeepalive(t.Context(), pglogrepl.PrimaryKeepaliveMessage{ServerWALEnd: 150})
	require.NoError(t, err)
	assert.True(t, yielded)
	assert.Nil(t, d.Change)
	assert.Equal(t, LSN(150), d.Delivered)

	_, _, err = s.handleWALData(walData(160, beginMessage(190)))
	require.NoError(t, err)
	d, yielded, err = s.handleKeepalive(t.Context(), pglogrepl.PrimaryKeepaliveMessage{ServerWALEnd: 200})
	require.NoError(t, err)
	assert.True(t, yielded)
	assert.Equal(t, LSN(150), d.Delivered, "inside a transaction the position holds")

	d, yielded, err = s.handleWALData(walData(190, commitMessage(190, 195)))
	require.NoError(t, err)
	assert.True(t, yielded)
	assert.Equal(t, LSN(195), d.Delivered, "the commit delivers the transaction's end")

	d, _, err = s.handleKeepalive(t.Context(), pglogrepl.PrimaryKeepaliveMessage{ServerWALEnd: 120})
	require.NoError(t, err)
	assert.Equal(t, LSN(195), d.Delivered, "a position already passed does not move it back")
}

// The server's position is kept from every message that carries it — a
// keepalive or a change — even inside a transaction, where Delivered holds,
// so a caller can measure the stream's lag without a second connection.
func TestServerWALEndFollowsEveryServerReport(t *testing.T) {
	s := &Stream{delivered: 100, relation: keyOnlyRelation(7)}
	assert.Equal(t, LSN(0), s.ServerWALEnd(), "nothing is known before the server reports")

	_, _, err := s.handleKeepalive(t.Context(), pglogrepl.PrimaryKeepaliveMessage{ServerWALEnd: 150})
	require.NoError(t, err)
	assert.Equal(t, LSN(150), s.ServerWALEnd())

	_, _, err = s.handleWALData(walData(160, beginMessage(190)))
	require.NoError(t, err)
	_, _, err = s.handleKeepalive(t.Context(), pglogrepl.PrimaryKeepaliveMessage{ServerWALEnd: 200})
	require.NoError(t, err)
	assert.Equal(t, LSN(200), s.ServerWALEnd(), "the server's position is kept inside a transaction")
	assert.Equal(t, LSN(150), s.Delivered(), "while the delivered position holds")

	xld := walData(170, insertMessage(7, "101"))
	xld.ServerWALEnd = 210
	_, _, err = s.handleWALData(xld)
	require.NoError(t, err)
	assert.Equal(t, LSN(210), s.ServerWALEnd(), "a change carries the server's position too")
}

// A change is stamped with the delivered position it arrives with — the
// stream's position before its transaction commits — which is what a caller
// may confirm while the change is unapplied; the change's own LSN is where
// it was written and may lie anywhere against that position.
func TestStreamStampsAChangeWithTheDeliveredPositionItArrivedWith(t *testing.T) {
	s := &Stream{delivered: 180, relation: keyOnlyRelation(7)}
	_, _, err := s.handleWALData(walData(160, beginMessage(190)))
	require.NoError(t, err)

	d, yielded, err := s.handleWALData(walData(170, insertMessage(7, "101")))

	require.NoError(t, err)
	require.True(t, yielded)
	assert.Equal(t, LSN(180), d.Delivered)
	assert.Equal(t, LSN(180), d.Change.Delivered)
	assert.Equal(t, LSN(170), d.Change.LSN, "the change's own position is where it was written")
	assert.EqualValues(t, 101, d.Change.Key)
}

// A row change the server sent outside BEGIN … COMMIT is refused rather
// than yielded: no commit would ever deliver a position covering it.
func TestStreamRefusesAChangeOutsideATransaction(t *testing.T) {
	s := &Stream{delivered: 100, relation: keyOnlyRelation(7)}

	d, yielded, err := s.handleWALData(walData(110, insertMessage(7, "101")))

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.False(t, yielded)
	assert.Nil(t, d.Change)
}

// A row change naming a relation other than the one the stream described
// is refused rather than decoded against the target's columns (ST-3).
func TestStreamRefusesAChangeToAnotherRelation(t *testing.T) {
	s := &Stream{delivered: 100, relation: keyOnlyRelation(7)}
	_, _, err := s.handleWALData(walData(105, beginMessage(140)))
	require.NoError(t, err)

	d, yielded, err := s.handleWALData(walData(110, insertMessage(8, "101")))

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.False(t, yielded)
	assert.Nil(t, d.Change)
}

// The server leaving copy-both mode in an orderly way — CopyDone, then the
// command's completion — is the stream ending, not state this package
// cannot have been handed; any other message outside copy-both mode is.
func TestStreamEndsWhenTheServerLeavesCopyBothMode(t *testing.T) {
	s := &Stream{delivered: 100, confirmed: 90}

	_, yielded, err := s.handleMessage(t.Context(), &pgproto3.CopyDone{})
	require.ErrorIs(t, err, ErrStreamEnded)
	assert.False(t, yielded)

	_, _, err = s.handleMessage(t.Context(), &pgproto3.CommandComplete{})
	require.ErrorIs(t, err, ErrStreamEnded)

	_, _, err = s.handleMessage(t.Context(), &pgproto3.ReadyForQuery{})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.NotErrorIs(t, err, ErrStreamEnded)
}

// Transaction boundaries must pair: a BEGIN inside a transaction or a
// COMMIT outside one is a stream this package cannot have been handed.
func TestUnpairedTransactionBoundariesStopTheStream(t *testing.T) {
	s := &Stream{delivered: 100}
	_, _, err := s.handleWALData(walData(110, commitMessage(110, 115)))
	require.ErrorIs(t, err, ErrInvariantViolation)

	s = &Stream{delivered: 100}
	_, _, err = s.handleWALData(walData(110, beginMessage(140)))
	require.NoError(t, err)
	_, _, err = s.handleWALData(walData(120, beginMessage(140)))
	require.ErrorIs(t, err, ErrInvariantViolation)
}

// Confirm refuses to move the slot backwards against its own record or
// forwards past what the caller has seen; both are refused before anything
// is sent.
func TestConfirmRefusesARegressionAndAnOverreach(t *testing.T) {
	s := &Stream{start: 100, delivered: 200, confirmed: 150}

	err := s.Confirm(t.Context(), 140)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, LSN(150), s.Confirmed())

	err = s.Confirm(t.Context(), 201)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, LSN(150), s.Confirmed())
}
