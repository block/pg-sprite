package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A shadow with no identity or sequence-owning column after the statement
// is admitted: the source's handed-off identity columns and copied serial
// defaults never appear in the list, so an empty list is the usual case.
func TestRefuseNewSequenceColumnsAdmitsAShadowWithoutOne(t *testing.T) {
	assert.NoError(t, refuseNewSequenceColumns("public", "orders", nil))
}

// A column the statement declared an identity is refused as a new sequence
// the swap cannot carry, and the message names the column.
func TestRefuseNewSequenceColumnsRefusesANewIdentityColumn(t *testing.T) {
	err := refuseNewSequenceColumns("public", "orders", []sequenceOwnedColumn{{column: "sku", identity: true}})

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementNewSequence, RefusalCauseOf(err))
	assert.ErrorContains(t, err, "sku", "the message names the column")
}

// A column the statement gave a sequence of its own — serial, or an
// explicit OWNED BY — is refused the same way.
func TestRefuseNewSequenceColumnsRefusesANewSerialColumn(t *testing.T) {
	err := refuseNewSequenceColumns("public", "orders", []sequenceOwnedColumn{{column: "seqno"}})

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementNewSequence, RefusalCauseOf(err))
	assert.ErrorContains(t, err, "seqno", "the message names the column")
}

// With two new sequence columns the message names the first in attribute
// order and gives that column's remedy: a serial column ahead of an
// identity column is told to attach a sequence, not to add an identity.
func TestRefuseNewSequenceColumnsNamesTheFirstColumnAndItsRemedy(t *testing.T) {
	err := refuseNewSequenceColumns("public", "orders", []sequenceOwnedColumn{
		{column: "seqno"},
		{column: "ticket", identity: true},
	})

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "seqno")
	assert.NotContains(t, err.Error(), "ticket", "only the first column is named")
	assert.ErrorContains(t, err, "CREATE SEQUENCE … OWNED BY", "a serial column's remedy attaches a sequence")
	assert.NotContains(t, err.Error(), "ADD GENERATED")

	err = refuseNewSequenceColumns("public", "orders", []sequenceOwnedColumn{
		{column: "ticket", identity: true},
		{column: "seqno"},
	})

	assert.ErrorContains(t, err, "ticket")
	assert.ErrorContains(t, err, "ADD GENERATED", "an identity column's remedy adds the identity")
	assert.NotContains(t, err.Error(), "OWNED BY")
}
