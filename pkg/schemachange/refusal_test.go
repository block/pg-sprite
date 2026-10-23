package schemachange

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/refusal"
)

// Every cause upholds the one invariant docs/invariants.md files it under —
// the lock causes under LK-1, the relation-identity causes under ST-6, the
// statement-target cause under ST-7, the fidelity and swap-readiness causes
// under ST-5, the proof cause under CO-1, the ambiguous-outcome cause
// under LK-4, the resume causes under ST-2, ST-3, and ST-4 — and an unknown
// cause upholds none. The refusal text leads
// with this identifier, so an operator looking it up must land on the right
// entry.
func TestEveryRefusalCauseNamesItsInvariant(t *testing.T) {
	want := map[RefusalCause]string{
		CauseLockUnproven:         "LK-1",
		CauseLockLost:             "LK-1",
		CauseLockHeldElsewhere:    "LK-1",
		CauseLockUnconfirmed:      "LK-1",
		CauseProofEmpty:           "ST-6",
		CauseSourceShape:          "ST-6",
		CauseRelationReplaced:     "ST-6",
		CauseStatementTarget:      "ST-7",
		CauseStatementConversion:  "ST-7",
		CauseShadowOwner:          "ST-5",
		CauseForeignRelation:      "ST-5",
		CauseGrantsDiffer:         "ST-5",
		CauseIdentityHandoff:      "ST-5",
		CauseSchemaDrift:          "ST-5",
		CauseFidelityDrift:        "ST-5",
		CauseIndexInvalid:         "ST-5",
		CauseNameTaken:            "ST-5",
		CauseCutoverUnverified:    "CO-1",
		CauseCutoverUndrained:     "CO-1",
		CauseCutoverUnfenced:      "CO-1",
		CauseDrainDeferred:        "CO-6",
		CauseSwapMismatch:         "ST-6",
		CauseOutcomeAmbiguous:     "LK-4",
		CauseResumeTerminal:       "ST-4",
		CauseResumeCutover:        "LK-4",
		CauseRestartSwapCommitted: "LK-4",
		CauseResumeRowMismatch:    "ST-2",
		CauseResumeRowDecoding:    "ST-3",
		CauseShadowUnrecorded:     "ST-2",
		CauseResumeShadowGone:     "ST-2",
		CauseResumeSlotForeign:    "ST-3",
		CauseResumeSlotHeld:       "ST-4",
		CauseCopyShadowMismatch:   "ST-6",
		CauseCopyFrontierBroken:   "CO-4",
		CauseCopyChunksInFlight:   "LK-3",
		CauseCopyChunkUnknown:     "ST-6",
	}
	assert.Len(t, RefusalCauses(), len(want), "every registered cause has an expected invariant")
	for _, cause := range RefusalCauses() {
		assert.Equal(t, want[cause], cause.Invariant(), "cause %q", cause)
	}
	assert.Empty(t, RefusalCause("not-a-cause").Invariant())
}

// Every cause routes to the class docs/refusal-classes.md decides for it.
// Lock, shape, ownership, drift, name, slot, and ambiguous-outcome causes
// are environmental, since the state they describe moved under the run and
// may not next time; the one statement the engine declines to carry is a
// capability boundary; a proof, row, statement pairing, or catalog the
// caller or the engine's own write got wrong is an invariant violation,
// since no retry changes it; every answer is a member of refusal.Classes(),
// so the closed set lists what the causes hand out. An unknown cause has no
// class.
func TestEveryRefusalCauseNamesItsClass(t *testing.T) {
	want := map[RefusalCause]refusal.Class{
		CauseLockUnproven:         refusal.ClassInvariantViolation,
		CauseLockLost:             refusal.ClassEnvironmental,
		CauseLockHeldElsewhere:    refusal.ClassEnvironmental,
		CauseLockUnconfirmed:      refusal.ClassEnvironmental,
		CauseProofEmpty:           refusal.ClassInvariantViolation,
		CauseSourceShape:          refusal.ClassEnvironmental,
		CauseStatementTarget:      refusal.ClassInvariantViolation,
		CauseStatementConversion:  refusal.ClassCapabilityBoundary,
		CauseShadowOwner:          refusal.ClassEnvironmental,
		CauseForeignRelation:      refusal.ClassEnvironmental,
		CauseGrantsDiffer:         refusal.ClassInvariantViolation,
		CauseIdentityHandoff:      refusal.ClassEnvironmental,
		CauseCutoverUnverified:    refusal.ClassInvariantViolation,
		CauseCutoverUndrained:     refusal.ClassInvariantViolation,
		CauseCutoverUnfenced:      refusal.ClassEnvironmental,
		CauseDrainDeferred:        refusal.ClassEnvironmental,
		CauseRelationReplaced:     refusal.ClassEnvironmental,
		CauseSchemaDrift:          refusal.ClassEnvironmental,
		CauseFidelityDrift:        refusal.ClassEnvironmental,
		CauseIndexInvalid:         refusal.ClassEnvironmental,
		CauseNameTaken:            refusal.ClassEnvironmental,
		CauseSwapMismatch:         refusal.ClassInvariantViolation,
		CauseOutcomeAmbiguous:     refusal.ClassEnvironmental,
		CauseResumeTerminal:       refusal.ClassInvariantViolation,
		CauseResumeCutover:        refusal.ClassInvariantViolation,
		CauseRestartSwapCommitted: refusal.ClassEnvironmental,
		CauseResumeRowMismatch:    refusal.ClassInvariantViolation,
		CauseResumeRowDecoding:    refusal.ClassInvariantViolation,
		CauseShadowUnrecorded:     refusal.ClassEnvironmental,
		CauseResumeShadowGone:     refusal.ClassEnvironmental,
		CauseResumeSlotForeign:    refusal.ClassEnvironmental,
		CauseResumeSlotHeld:       refusal.ClassEnvironmental,
		CauseCopyShadowMismatch:   refusal.ClassInvariantViolation,
		CauseCopyFrontierBroken:   refusal.ClassInvariantViolation,
		CauseCopyChunksInFlight:   refusal.ClassInvariantViolation,
		CauseCopyChunkUnknown:     refusal.ClassInvariantViolation,
	}
	assert.Len(t, RefusalCauses(), len(want), "every registered cause has an expected class")
	for _, cause := range RefusalCauses() {
		assert.Equal(t, want[cause], cause.Class(), "cause %q", cause)
		assert.Contains(t, refusal.Classes(), cause.Class(), "cause %q answers a class refusal.Classes() does not list", cause)
	}
	assert.Empty(t, RefusalCause("not-a-cause").Class())
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
