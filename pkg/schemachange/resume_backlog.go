package schemachange

import (
	"github.com/block/pg-sprite/pkg/decode"
)

// checkResumeBacklog refuses a clean live resume whose slot, as
// InspectResume read it, already holds more WAL its stream has not
// confirmed than ceiling, the run's slot lag ceiling. The catch-up judges
// that measure against the same ceiling on its first cycle, before the
// reopened stream can have read through such a backlog, so the resume
// would stop stream-behind there, after its copy had already cleared and
// begun refilling the shadow and added WAL of its own to the slot. It is
// judged before the stream opens or the copy restarts, so the refusal
// writes nothing and the slot is left as found.
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
	return refuse(CauseResumeSlotBacklog, nil,
		"slot %s holds %d bytes of WAL its stream has not confirmed, over the %d-byte SlotLagCeiling, "+
			"so the resume would stop stream-behind on its first catch-up cycle; nothing was written and the slot is left as found: "+
			"resume with a SlotLagCeiling above that WAL and room on the volume for it, "+
			"or run the statement again with a restart, which drops the slot and the WAL it holds",
		status.Name, status.Unconfirmed.Bytes, ceiling)
}
