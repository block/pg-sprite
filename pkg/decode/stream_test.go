package decode

import (
	"encoding/binary"
	"testing"

	"github.com/jackc/pglogrepl"
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

// A keepalive's position is the decoder's send position, which can lie
// past changes of an open transaction that have not been yielded yet; it
// is delivered only between transactions. A keepalive that asks for no
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
