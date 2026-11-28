package executor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A node from a partition listing is not the standalone table the step
// works on, and is refused before the step reads anything.
func TestEnsureAttachForeignKeysRefusesANodeThatIsNotStandalone(t *testing.T) {
	node := partitionNode{oid: 1, parentOID: 2, schema: "app", name: "payments_2027", relkind: "r"}
	_, err := ensureAttachForeignKeys(t.Context(), nil, 3, node, ValidateBudget{LockTimeout: time.Second, Overall: time.Minute}, nil)
	require.ErrorIs(t, err, ErrInvariantViolation)
}

// A budget with no bounds is refused with the budget's own refusal before
// the step reads anything, even for a parent that would turn out to have no
// foreign key.
func TestEnsureAttachForeignKeysRefusesAnUnboundedBudget(t *testing.T) {
	node := standaloneTable(1, "app", "payments_2027")
	_, err := ensureAttachForeignKeys(t.Context(), nil, 3, node, ValidateBudget{}, nil)
	require.Error(t, err)
	assert.Equal(t, ValidateBudget{}.validate(), err)
}

// The referenced table is resolved by name for the leaf step, so a name
// that now resolves to a table other than the one the parent's foreign key
// references is refused as an invariant violation: the attach would not
// adopt a foreign key to it. The same table is taken.
func TestProveParentFKReference(t *testing.T) {
	pfk := parentForeignKey{
		spec:    partitionedFKSpec{name: "payments_account_fk", refSchema: "app", refTable: "accounts"},
		catalog: catalogFK{refOID: 100},
	}
	assert.NoError(t, proveParentFKReference(pfk, fkReference{oid: 100, schema: "app", table: "accounts"}))
	assert.ErrorIs(t, proveParentFKReference(pfk, fkReference{oid: 200, schema: "app", table: "accounts"}), ErrInvariantViolation)
}
