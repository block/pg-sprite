package decode

import (
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerTarget is the target the ledger relation was proven for: the names
// and OID pgoutput reports for it, and the key column.
type ledgerTarget struct{ oid uint32 }

func (ledgerTarget) Schema() string   { return "app" }
func (ledgerTarget) Table() string    { return "ledger" }
func (t ledgerTarget) OID() uint32    { return t.oid }
func (ledgerTarget) PKColumn() string { return "id" }

// ledgerMessage describes the ledger table as pgoutput does, under the OID
// the server gave it.
func ledgerMessage(oid uint32) *pglogrepl.RelationMessage {
	return &pglogrepl.RelationMessage{
		RelationID:      oid,
		Namespace:       "app",
		RelationName:    "ledger",
		ReplicaIdentity: 'd',
		Columns: []*pglogrepl.RelationMessageColumn{
			{Flags: keyFlag, Name: "id", DataType: 20},
			{Name: "note", DataType: 25},
		},
	}
}

// A relation message is the target only when it carries the target's names
// and the OID the target was proven under. The same names under another
// OID are a table dropped and recreated under them, or a table the pool's
// proof never saw, and are refused before any of its changes are decoded.
func TestNewRelationRefusesTheTargetsNameUnderAnotherOID(t *testing.T) {
	rel, err := newRelation(ledgerMessage(16384), ledgerTarget{oid: 16384})
	require.NoError(t, err)
	assert.EqualValues(t, 16384, rel.id)
	assert.Equal(t, 0, rel.keyIndex)

	_, err = newRelation(ledgerMessage(16401), ledgerTarget{oid: 16384})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "OID 16384")
	assert.ErrorContains(t, err, "OID 16401")
}

// Another relation under the publication is state this package did not
// create, and is refused by name before its OID is read.
func TestNewRelationRefusesAnotherRelation(t *testing.T) {
	msg := ledgerMessage(16384)
	msg.RelationName = "ledger_archive"

	_, err := newRelation(msg, ledgerTarget{oid: 16384})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "app.ledger_archive")
}
