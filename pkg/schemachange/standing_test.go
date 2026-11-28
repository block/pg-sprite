package schemachange

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
)

// A run the cutover gate stops on a changed table keeps its refusal, cause
// and wrappers alike, and carries what the row says the run leaves; a
// refusal the run can be resumed past carries nothing more.
func TestWithStandingRunNamesWhatADriftStopLeaves(t *testing.T) {
	cp := checkpoint.Checkpoint{Schema: "public", Table: "c200", ShadowTable: "_pgsprite_c200_new",
		SlotName: "pgsprite_c200", PublicationName: "pgsprite_c200", Phase: checkpoint.PhaseVerifying}
	for _, cause := range []RefusalCause{CauseSchemaDrift, CauseFidelityDrift} {
		gate := fmt.Errorf("gate: %w", refuse(cause, nil, "source public.c200 changed"))
		err := withStandingRun(gate, cp)
		assert.Equal(t, cause, RefusalCauseOf(err))
		assert.ErrorIs(t, err, ErrInvariantViolation)
		standing, ok := errors.AsType[*StandingRunError](err)
		require.True(t, ok, "cause %s carries what the run leaves", cause)
		assert.Equal(t, StandingRunError{Schema: "public", Table: "c200", Shadow: "_pgsprite_c200_new",
			Slot: "pgsprite_c200", Publication: "pgsprite_c200", Phase: checkpoint.PhaseVerifying}, *standing)
	}

	held := refuse(CauseResumeSlotHeld, nil, "slot pgsprite_c200 is held by backend 7")
	assert.Same(t, held, withStandingRun(held, cp))
}

// The leftovers name the hold the run's kind keeps: a live run's slot,
// which retains WAL, and its publication; a quiesced run's write fence.
func TestStandingRunLeftoversNameTheHold(t *testing.T) {
	live := StandingRunError{Schema: "public", Table: "c200", Shadow: "_pgsprite_c200_new",
		Slot: "pgsprite_c200", Publication: "pgsprite_c200", Phase: checkpoint.PhaseVerifying}
	assert.Equal(t, "shadow table public._pgsprite_c200_new, replication slot pgsprite_c200, which retains WAL on the server until it is dropped, "+
		"publication pgsprite_c200, and the checkpoint row in phase verifying", live.Leftovers())

	quiesced := StandingRunError{Schema: "public", Table: "c200", Shadow: "_pgsprite_c200_new", Phase: checkpoint.PhaseCopying}
	assert.Equal(t, "shadow table public._pgsprite_c200_new, the write fence on public.c200, and the checkpoint row in phase copying", quiesced.Leftovers())
}
