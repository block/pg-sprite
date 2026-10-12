package schemachange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ResumeMode is how a run picks up a checkpoint row it did not finish.
type ResumeMode uint8

const (
	// ResumeClean means the row's slot is intact and readable: the run
	// reopens the stream at the row's last applied position and continues
	// the phase the row records.
	ResumeClean ResumeMode = iota + 1
	// ResumeReconcile means change capture cannot continue from the row's
	// slot (ST-4): the run keeps the shadow and the watermark and goes
	// through Reconciler.Reconcile before any stream is opened.
	ResumeReconcile
)

// String returns the stable mode name.
func (m ResumeMode) String() string {
	switch m {
	case ResumeClean:
		return "clean"
	case ResumeReconcile:
		return "reconcile"
	default:
		return fmt.Sprintf("ResumeMode(%d)", m)
	}
}

// Resume is InspectResume's reading of a checkpoint row against the
// catalog: the mode, and the slot state that decided it. What decided the
// mode is readable from the fields:
//
//	Mode             Lost      Found  Slot        the row's slot
//	ResumeClean      nil       false  zero        none to read: the target does not decode WAL
//	ResumeClean      nil       true   as read     present, served, and held by nobody
//	ResumeReconcile  nil       either as read     the row is already reconciling, whatever the catalog shows
//	ResumeReconcile  non-nil   false  zero        the row names no slot, or no slot of the name exists
//	ResumeReconcile  non-nil   true   as read     the server reports the slot lost
type Resume struct {
	Mode ResumeMode
	// Lost is why the row's slot cannot be decoded from, when that is what
	// decided the mode: the slot no longer exists, or the server reports
	// it lost, with the server's cause when it gives one. It is nil for a
	// clean resume, and for a row already in PhaseReconciling, whose slot
	// is not to be trusted whatever the catalog says of it.
	Lost *decode.SlotLostError
	// Slot is the row's slot as the catalog has it, when Found.
	Slot  decode.SlotStatus
	Found bool
}

// InspectResume decides how a run resumes from a checkpoint row for
// target. A target that does not decode WAL has no slot to lose and always
// resumes clean. Otherwise a row in PhaseReconciling is reconciled again
// from the top: the phase is written before the lost slot is dropped, so
// the row says a reconcile started and nothing says it finished, whatever
// slot the catalog shows under the name. Any other row's slot is read on
// pool: a slot that no longer exists or that the server reports lost means
// reconcile, with the server's verdict in Lost; a slot the server still
// serves and nothing holds means a clean resume. A row that names no slot
// — saved before its slot was created — is reconciled too, since nothing
// captured the changes behind what landed.
//
// Refusals come before any slot is touched. A row in a terminal phase has
// nothing to resume (resume-row-terminal) — a caller that reads
// cp.Phase.Terminal() first, as the ordinary end of a run, never meets
// that refusal; a row in the cutover phase is
// the swap's to answer for — InspectSwapped reads whether it committed —
// and is resume-row-cutover until the caller has that answer; a row that
// does not describe target is resume-row-mismatch. A slot of the derived
// name that is not this route's — physical, or another database's — is
// resume-slot-foreign in every phase, since neither a clean resume nor a
// reconcile may decode from or replace it, whatever its WAL status says. A
// slot of the route's own that another backend holds is resume-slot-held
// rather than clean, since no stream opens on it until the holder lets go;
// the caller inspects again once it has.
func InspectResume(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) (Resume, error) {
	if err := checkProof(target); err != nil {
		return Resume{}, err
	}
	if err := checkResumeRow(target, cp); err != nil {
		return Resume{}, err
	}
	if !target.DecodesWAL() {
		return Resume{Mode: ResumeClean}, nil
	}
	// INV: ST-4 — a reconcile that started is finished before capture resumes.
	if cp.Phase == checkpoint.PhaseReconciling {
		status, found, err := decode.InspectSlot(ctx, pool, target.DecodingName())
		if err != nil {
			return Resume{}, fmt.Errorf("inspect resume of %s.%s: %w", cp.Schema, cp.Table, err)
		}
		if found {
			if err := checkResumeSlot(status); err != nil {
				return Resume{}, err
			}
		}
		return Resume{Mode: ResumeReconcile, Slot: status, Found: found}, nil
	}
	if cp.SlotName == "" {
		return Resume{Mode: ResumeReconcile, Lost: &decode.SlotLostError{Slot: target.DecodingName()}}, nil
	}
	status, found, err := decode.InspectSlot(ctx, pool, cp.SlotName)
	if err != nil {
		return Resume{}, fmt.Errorf("inspect resume of %s.%s: %w", cp.Schema, cp.Table, err)
	}
	// INV: ST-4 — a vanished slot is the lost state, not a clean resume.
	if !found {
		return Resume{Mode: ResumeReconcile, Lost: &decode.SlotLostError{Slot: cp.SlotName}}, nil
	}
	if err := checkResumeSlot(status); err != nil {
		return Resume{}, err
	}
	// INV: ST-4 — lost is the server's verdict, read from the catalog.
	if lost := status.LostState(); lost != nil {
		return Resume{Mode: ResumeReconcile, Lost: lost, Slot: status, Found: true}, nil
	}
	if status.Active {
		// INV: ST-4 — a held slot is not readable now, and is not lost.
		return Resume{}, refuse(CauseResumeSlotHeld, nil, "slot %s is held by backend %d", status.Name, status.ActivePID)
	}
	return Resume{Mode: ResumeClean, Slot: status, Found: true}, nil
}

// checkResumeSlot refuses a slot of the derived name that is not the
// route's own: one this run neither decodes from nor, in reconcile mode,
// drops and replaces. It is judged before the slot's WAL status, since a
// foreign slot the server reports lost is still foreign.
func checkResumeSlot(status decode.SlotStatus) error {
	if status.Own {
		return nil
	}
	// INV: ST-3 — foreign state of the derived name is never adopted.
	foreign := &decode.ForeignStateError{Object: decode.ForeignObjectSlot, Name: status.Name,
		Detail: "it belongs to another database or is a physical slot"}
	return refuse(CauseResumeSlotForeign, []error{foreign}, "the slot of the derived name is not this route's")
}

// checkResumeRow refuses a checkpoint row that a resume for target cannot
// act on: one in a terminal phase, one in the cutover phase — whose swap
// may have committed, which only InspectSwapped can tell — or one keyed on
// another table or naming a slot other than the target's derived name. The
// shadow and fingerprint checks need the built shadow and belong to the
// operation that has it.
func checkResumeRow(target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	if cp.Phase.Terminal() {
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s", cp.Schema, cp.Table, cp.Phase)
	}
	// INV: LK-4 — a cutover row is answered by InspectSwapped, never resumed past.
	if cp.Phase == checkpoint.PhaseCutover {
		return refuse(CauseResumeCutover, nil, "checkpoint row for %s.%s is in phase cutover; InspectSwapped reads whether the swap committed", cp.Schema, cp.Table)
	}
	if cp.Schema != target.Schema() || cp.Table != target.Table() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row is for %s.%s, target is %s.%s", cp.Schema, cp.Table, target.Schema(), target.Table())
	}
	if cp.SlotName != "" && cp.SlotName != target.DecodingName() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names slot %s, the target's slot is %s", cp.Schema, cp.Table, cp.SlotName, target.DecodingName())
	}
	return nil
}
