package copier

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/refusal"
)

// Every cause upholds the one invariant docs/invariants.md files it under —
// the proof, shadow, relation, and feedback causes under ST-6, the lock
// causes under LK-1, the frontier cause under CO-4, the in-flight cause
// under LK-3 — and an unknown cause upholds none. The refusal text leads
// with this identifier, so an operator looking it up must land on the right
// entry.
func TestEveryRefusalCauseNamesItsInvariant(t *testing.T) {
	want := map[RefusalCause]string{
		CauseProofEmpty:        "ST-6",
		CauseShadowMismatch:    "ST-6",
		CauseRelationReplaced:  "ST-6",
		CauseChunkUnknown:      "ST-6",
		CauseLockUnproven:      "LK-1",
		CauseLockLost:          "LK-1",
		CauseLockHeldElsewhere: "LK-1",
		CauseLockUnconfirmed:   "LK-1",
		CauseFrontierBroken:    "CO-4",
		CauseChunksInFlight:    "LK-3",
	}
	assert.Len(t, RefusalCauses(), len(want), "every registered cause has an expected invariant")
	for _, cause := range RefusalCauses() {
		assert.Equal(t, want[cause], cause.Invariant(), "cause %q", cause)
	}
	assert.Empty(t, RefusalCause("not-a-cause").Invariant())
}

// Every cause routes to the class docs/refusal-classes.md decides for it:
// the three lock faults the server or the session reported and a relation
// that moved are environmental, since re-acquiring or re-running preflight
// may admit the same copy; every proof, shadow, chunk, and ledger cause is
// an invariant violation, since no retry changes a caller's bad input or
// the copier's own broken accounting. An unknown cause has no class.
func TestEveryRefusalCauseNamesItsClass(t *testing.T) {
	want := map[RefusalCause]refusal.Class{
		CauseProofEmpty:        refusal.ClassInvariantViolation,
		CauseShadowMismatch:    refusal.ClassInvariantViolation,
		CauseLockUnproven:      refusal.ClassInvariantViolation,
		CauseLockLost:          refusal.ClassEnvironmental,
		CauseLockHeldElsewhere: refusal.ClassEnvironmental,
		CauseLockUnconfirmed:   refusal.ClassEnvironmental,
		CauseRelationReplaced:  refusal.ClassEnvironmental,
		CauseFrontierBroken:    refusal.ClassInvariantViolation,
		CauseChunksInFlight:    refusal.ClassInvariantViolation,
		CauseChunkUnknown:      refusal.ClassInvariantViolation,
	}
	assert.Len(t, RefusalCauses(), len(want), "every registered cause has an expected class")
	for _, cause := range RefusalCauses() {
		assert.Equal(t, want[cause], cause.Class(), "cause %q", cause)
	}
	assert.Empty(t, RefusalCause("not-a-cause").Class())
}

// A refusal answers errors.Is for the sentinel and for every error behind
// it, and RefusalCauseOf reads the cause through any wrapping; an error that
// is not a refusal has no cause, the bare sentinel included.
func TestRefusalCauseOfReadsThroughWrapping(t *testing.T) {
	lost := errors.New("lock session backend terminated")
	statement := errors.New("statement cancelled")
	refusal := refuse(CauseLockLost, []error{lost, statement}, "table lock was lost during the copy")

	assert.ErrorIs(t, refusal, ErrInvariantViolation)
	assert.ErrorIs(t, refusal, lost)
	assert.ErrorIs(t, refusal, statement)
	assert.Equal(t, CauseLockLost, RefusalCauseOf(refusal))
	assert.Equal(t, CauseLockLost, RefusalCauseOf(fmt.Errorf("copy: %w", refusal)))

	assert.Empty(t, RefusalCauseOf(nil))
	assert.Empty(t, RefusalCauseOf(ErrInvalidOptions))
	assert.Empty(t, RefusalCauseOf(fmt.Errorf("%w: sales.widgets", ErrInvariantViolation)))
}

// The refusal's text is its own renderer: it leads with the sentinel, then
// the invariant and cause a reader can look up, then the detail with its
// arguments formatted in, then the errors behind it in order.
func TestRefusalErrorRendersCauseThenDetailThenWrapped(t *testing.T) {
	refusal := refuse(CauseFrontierBroken, nil, "claimed chunk [%d, %d] does not start at the cut frontier", 1001, 2000)
	assert.Equal(t,
		"invariant violation: CO-4 (copy-frontier-broken): claimed chunk [1001, 2000] does not start at the cut frontier",
		refusal.Error())

	gone := errors.New("relation does not exist")
	refusal = refuse(CauseRelationReplaced, []error{gone}, "%s or its shadow %s no longer exists", `"sales"."widgets"`, `"sales"."_pgsprite_widgets_new"`)
	assert.Equal(t,
		`invariant violation: ST-6 (copy-relation-replaced): "sales"."widgets" or its shadow "sales"."_pgsprite_widgets_new" no longer exists: relation does not exist`,
		refusal.Error())
}
