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
// catalog: the mode, and the slot state that decided it.
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
// target. A row in PhaseReconciling is reconciled again from the top: the
// phase is written before the lost slot is dropped, so the row says a
// reconcile started and nothing says it finished, whatever slot the catalog
// shows under the name. Otherwise the row's slot is read on pool: a slot
// that no longer exists or that the server reports lost means reconcile,
// with the server's verdict in Lost; a readable slot means a clean resume.
// A row that names no slot for a target that decodes WAL — saved before
// its slot was created — is reconciled too, since nothing captured the
// changes behind what landed; a target that does not decode WAL has no
// slot to lose and always resumes clean. A row in a terminal phase has
// nothing to resume and is refused as resume-row-terminal; a row that
// does not describe target is refused as resume-row-mismatch.
func InspectResume(ctx context.Context, pool *pgxpool.Pool, target preflight.CopySwapTarget, cp checkpoint.Checkpoint) (Resume, error) {
	if err := checkProof(target); err != nil {
		return Resume{}, err
	}
	if err := checkResumeRow(target, cp); err != nil {
		return Resume{}, err
	}
	// INV: ST-4 — a reconcile that started is finished before capture resumes.
	if cp.Phase == checkpoint.PhaseReconciling {
		status, found, err := decode.InspectSlot(ctx, pool, target.DecodingName())
		if err != nil {
			return Resume{}, fmt.Errorf("inspect resume of %s.%s: %w", cp.Schema, cp.Table, err)
		}
		return Resume{Mode: ResumeReconcile, Slot: status, Found: found}, nil
	}
	if !target.DecodesWAL() {
		return Resume{Mode: ResumeClean}, nil
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
	// INV: ST-4 — lost is the server's verdict, read from the catalog.
	if lost := status.LostState(); lost != nil {
		return Resume{Mode: ResumeReconcile, Lost: lost, Slot: status, Found: true}, nil
	}
	return Resume{Mode: ResumeClean, Slot: status, Found: true}, nil
}

// checkResumeRow refuses a checkpoint row that a resume for target cannot
// act on: one in a terminal phase, or one keyed on another table or naming
// a slot other than the target's derived name. The shadow and fingerprint
// checks need the built shadow and belong to the operation that has it.
func checkResumeRow(target preflight.CopySwapTarget, cp checkpoint.Checkpoint) error {
	if cp.Phase.Terminal() {
		return refuse(CauseResumeTerminal, nil, "checkpoint row for %s.%s is in phase %s", cp.Schema, cp.Table, cp.Phase)
	}
	if cp.Schema != target.Schema() || cp.Table != target.Table() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row is for %s.%s, target is %s.%s", cp.Schema, cp.Table, target.Schema(), target.Table())
	}
	if cp.SlotName != "" && cp.SlotName != target.DecodingName() {
		return refuse(CauseResumeRowMismatch, nil, "checkpoint row for %s.%s names slot %s, the target's slot is %s", cp.Schema, cp.Table, cp.SlotName, target.DecodingName())
	}
	return nil
}
