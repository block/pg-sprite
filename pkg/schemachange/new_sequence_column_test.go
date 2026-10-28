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
