package schemachange

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/checkpoint"
)

// StandingRunError names what a run that stopped short of its swap leaves
// standing when it stops on a table changed under it: the shadow it
// built, its hold on the source, and its checkpoint row. None of them is
// released by the stop, and the run cannot go on as it is, so they stand
// until the run is started over (RunOptions.Restart) or discarded
// (Runner.Discard). A live run's slot keeps the server's WAL from the
// row's applied position on for as long as it stands. It rides beside
// the refusal that stopped the run, read with errors.As, so a front door
// can name the leftovers and its own spelling of the two ways past them
// without parsing the refusal's text.
type StandingRunError struct {
	// Schema and Table name the source the run changes.
	Schema string
	Table  string
	// Shadow is the shadow table the run built, in Schema.
	Shadow string
	// Slot and Publication are a live run's replication slot and
	// publication. Both are empty for a run that blocked writes, whose
	// hold is the write fence on the source.
	Slot        string
	Publication string
	// Phase is the phase the checkpoint row stands in.
	Phase checkpoint.Phase
}

// Error implements the error interface.
func (e *StandingRunError) Error() string {
	return fmt.Sprintf("the stopped run leaves %s; start it over with RunOptions.Restart or end it with Runner.Discard", e.Leftovers())
}

// Leftovers lists what the stopped run leaves standing, in the order a
// discard drops it: the shadow, the hold, the row.
func (e *StandingRunError) Leftovers() string {
	hold := fmt.Sprintf("the write fence on %s.%s", e.Schema, e.Table)
	if e.Slot != "" {
		hold = fmt.Sprintf("replication slot %s, which retains WAL on the server until it is dropped", e.Slot)
		if e.Publication != "" {
			hold += fmt.Sprintf(", publication %s", e.Publication)
		}
	}
	return fmt.Sprintf("shadow table %s.%s, %s, and the checkpoint row in phase %s", e.Schema, e.Shadow, hold, e.Phase)
}

// standingRun is what the run cp records leaves standing.
func standingRun(cp checkpoint.Checkpoint) *StandingRunError {
	return &StandingRunError{
		Schema:      cp.Schema,
		Table:       cp.Table,
		Shadow:      cp.ShadowTable,
		Slot:        cp.SlotName,
		Publication: cp.PublicationName,
		Phase:       cp.Phase,
	}
}

// withStandingRun adds to a refusal that stops the run on a table changed
// under it at the cutover gate — cutover-schema-drift or
// cutover-fidelity-drift — what the run cp records leaves standing, so the
// caller learns what it is paying for and how to get past it. The refusal
// is kept as returned, wrappers and all; any other error is returned
// unchanged.
func withStandingRun(err error, cp checkpoint.Checkpoint) error {
	switch RefusalCauseOf(err) {
	case CauseSchemaDrift, CauseFidelityDrift:
		return fmt.Errorf("%w; %w", err, standingRun(cp))
	default:
		return err
	}
}
