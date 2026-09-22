package schemachange

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every cause upholds a registered invariant; an unknown cause upholds none.
func TestEveryRefusalCauseNamesItsInvariant(t *testing.T) {
	registered := map[string]bool{"LK-1": true, "ST-5": true, "ST-6": true, "ST-7": true}
	for _, cause := range RefusalCauses() {
		assert.True(t, registered[cause.Invariant()], "cause %q names invariant %q", cause, cause.Invariant())
	}
	assert.Empty(t, RefusalCause("not-a-cause").Invariant())
}

// A refusal answers errors.Is for the sentinel and for every error behind
// it, and RefusalCauseOf reads the cause through any wrapping; an error that
// is not a refusal has no cause, the bare sentinel included.
func TestRefusalCauseOfReadsThroughWrapping(t *testing.T) {
	lost := errors.New("lock session backend terminated")
	statement := errors.New("statement cancelled")
	refusal := refuse(CauseLockLost, []error{lost, statement}, "table lock was lost during the shadow operation")

	assert.ErrorIs(t, refusal, ErrInvariantViolation)
	assert.ErrorIs(t, refusal, lost)
	assert.ErrorIs(t, refusal, statement)
	assert.Equal(t, CauseLockLost, RefusalCauseOf(refusal))
	assert.Equal(t, CauseLockLost, RefusalCauseOf(fmt.Errorf("resume: %w", refusal)))

	assert.Empty(t, RefusalCauseOf(nil))
	assert.Empty(t, RefusalCauseOf(ErrShadowNotFound))
	assert.Empty(t, RefusalCauseOf(fmt.Errorf("%w: sales.widgets", ErrInvariantViolation)))
}

// The refusal's text is its own renderer: it leads with the sentinel, then
// the invariant and cause a reader can look up, then the detail with its
// arguments formatted in, then the errors behind it in order.
func TestRefusalErrorRendersCauseThenDetailThenWrapped(t *testing.T) {
	refusal := refuse(CauseLockHeldElsewhere, nil, "table lock on %s.%s is held by backend %d, not the lock session's backend %d", "sales", "widgets", 41, 40)
	assert.Equal(t,
		"invariant violation: LK-1 (shadow-lock-held-elsewhere): table lock on sales.widgets is held by backend 41, not the lock session's backend 40",
		refusal.Error())

	wrapped := refuse(CauseLockLost, []error{errors.New("backend gone"), errors.New("statement cancelled")}, "table lock was lost during the shadow operation")
	assert.Equal(t,
		"invariant violation: LK-1 (shadow-lock-lost): table lock was lost during the shadow operation: backend gone: statement cancelled",
		wrapped.Error())
}
