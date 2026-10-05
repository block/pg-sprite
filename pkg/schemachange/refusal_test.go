package schemachange

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every cause upholds the one invariant docs/invariants.md files it under —
// the lock causes under LK-1, the relation-identity causes under ST-6, the
// statement-target cause under ST-7, the fidelity and swap-readiness causes
// under ST-5, the proof cause under CO-1, and the ambiguous-outcome cause
// under LK-4 — and an unknown cause upholds none. The refusal text leads
// with this identifier, so an operator looking it up must land on the right
// entry.
func TestEveryRefusalCauseNamesItsInvariant(t *testing.T) {
	want := map[RefusalCause]string{
		CauseLockUnproven:      "LK-1",
		CauseLockLost:          "LK-1",
		CauseLockHeldElsewhere: "LK-1",
		CauseLockUnconfirmed:   "LK-1",
		CauseProofEmpty:        "ST-6",
		CauseSourceShape:       "ST-6",
		CauseRelationReplaced:  "ST-6",
		CauseStatementTarget:   "ST-7",
		CauseShadowOwner:       "ST-5",
		CauseForeignRelation:   "ST-5",
		CauseGrantsDiffer:      "ST-5",
		CauseIdentityHandoff:   "ST-5",
		CauseSchemaDrift:       "ST-5",
		CauseFidelityDrift:     "ST-5",
		CauseIndexInvalid:      "ST-5",
		CauseNameTaken:         "ST-5",
		CauseCutoverUnverified: "CO-1",
		CauseSwapMismatch:      "ST-6",
		CauseOutcomeAmbiguous:  "LK-4",
	}
	assert.Len(t, RefusalCauses(), len(want), "every registered cause has an expected invariant")
	for _, cause := range RefusalCauses() {
		assert.Equal(t, want[cause], cause.Invariant(), "cause %q", cause)
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
