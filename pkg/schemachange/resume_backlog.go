package schemachange

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/decode"
)

// SlotBacklogError is the slot reading a resume-slot-backlog refusal
// wraps, so a host can build its own remedy with errors.As instead of
// reading the refusal's text.
type SlotBacklogError struct {
	// Slot is the replication slot the resume would have decoded from.
	Slot string
	// Unconfirmed is the WAL, in bytes, between the server's write
	// position and the slot's confirmed position when the resume read
	// it. It grows while the run stays stopped.
	Unconfirmed int64
	// Ceiling is the run's slot lag ceiling, in bytes, that Unconfirmed
	// is over.
	Ceiling int64
}

// Error implements the error interface.
func (e *SlotBacklogError) Error() string {
	return fmt.Sprintf("slot %s holds %d bytes of WAL its stream has not confirmed, over the %d-byte SlotLagCeiling",
		e.Slot, e.Unconfirmed, e.Ceiling)
}

// checkResumeBacklog refuses a clean live resume whose slot, as
// InspectResume read it, already holds more WAL its stream has not
// confirmed than ceiling, the run's slot lag ceiling. The catch-up judges
// that measure against the same ceiling at the end of its first cycle,
// and a stream reopened that far behind is not expected to read through
// the backlog within one cycle, so the resume would stop stream-behind
// there, after its copy had already cleared and begun refilling the
// shadow and added WAL of its own to the slot.
//
// It is a check on the slot as it stands when the resume starts, not a
// promise about the first cycle: the cluster's writes during that cycle
// add to the measure, so a resume admitted close to the ceiling can still
// stop stream-behind. It is judged before the stream opens or the copy
// restarts, so a refused resume clears and copies no shadow row and its
// slot confirms nothing; a row stopped in cutover whose swap rolled back
// has already been written back to catching_up (swapLive), the phase the
// resume would write anyway.
//
// An unconfirmed measure the server could not take, on a server in
// recovery, proves no backlog and is not refused here; it is not read as
// nothing unconfirmed either, since the catch-up's first reading fails
// closed on it (ErrSlotLagUnknown).
func checkResumeBacklog(status decode.SlotStatus, ceiling int64) error {
	if !status.Unconfirmed.Known {
		return nil
	}
	if status.Unconfirmed.Bytes <= ceiling {
		return nil
	}
	// INV: ST-3 — a resume whose slot is already over the ceiling is
	// refused before it adds WAL to the slot.
	backlog := &SlotBacklogError{Slot: status.Name, Unconfirmed: status.Unconfirmed.Bytes, Ceiling: ceiling}
	return refuse(CauseResumeSlotBacklog, []error{backlog},
		"the slot is over the run's slot lag ceiling before the resume opens its stream, so no shadow row was cleared or copied and the slot confirmed nothing")
}
