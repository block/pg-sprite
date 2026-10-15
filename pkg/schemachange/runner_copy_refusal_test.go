package schemachange

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// Every copier cause is re-minted as a shadow cause that upholds the same
// invariant: a copier cause added without a mapping, or mapped across
// invariants, fails here.
func TestEveryCopierCauseIsRemintedUnderTheSameInvariant(t *testing.T) {
	assert.Empty(t, copierCause(""), "the empty copier cause is no refusal")
	assert.Empty(t, copierCause("not-a-cause"))
	for _, cause := range copier.RefusalCauses() {
		mapped := copierCause(cause)
		require.NotEmpty(t, mapped, "copier cause %q has no shadow cause", cause)
		assert.Contains(t, RefusalCauses(), mapped, "copier cause %q maps to unregistered %q", cause, mapped)
		assert.Equal(t, cause.Invariant(), mapped.Invariant(), "copier cause %q and %q uphold different invariants", cause, mapped)
	}
}

// A re-minted copier refusal answers in both vocabularies and still
// reaches the copier's own refusal; an error that is not a copier refusal
// passes through untouched.
func TestCopyRefusalRemintsCopierRefusalsOnly(t *testing.T) {
	copierErr := &copier.RefusalError{Cause: copier.CauseLockLost, Detail: "table lock was lost during chunk [1,1000)"}
	err := copyRefusal(copierErr, "sales", "widgets")
	assert.Equal(t, CauseLockLost, RefusalCauseOf(err))
	assert.Equal(t, copier.CauseLockLost, copier.RefusalCauseOf(err))
	assert.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorIs(t, err, copierErr)
	assert.Contains(t, err.Error(), "copy of sales.widgets refused")
	assert.Contains(t, err.Error(), copierErr.Detail)

	server := errors.New("connection reset")
	assert.Same(t, server, copyRefusal(server, "sales", "widgets"))
	assert.NoError(t, copyRefusal(nil, "sales", "widgets"))
}
