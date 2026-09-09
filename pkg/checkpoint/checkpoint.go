package checkpoint

import (
	"fmt"
	"time"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// Phase identifies the durable schema-change phase. Values are opaque
// labels: the happy path visits them in declaration order, but PhaseFailed
// is reachable from any phase, so callers never compare phases for order —
// they switch on the value or ask Terminal.
type Phase uint8

const (
	// PhaseCopying copies source rows.
	PhaseCopying Phase = iota + 1
	// PhaseCatchingUp applies captured changes.
	PhaseCatchingUp
	// PhaseVerifying verifies convergence.
	PhaseVerifying
	// PhaseCutover performs the swap.
	PhaseCutover
	// PhaseDone is successful and terminal.
	PhaseDone
	// PhaseFailed is unsuccessful and terminal.
	PhaseFailed
)

// Terminal reports whether the phase is an end state that resume must not
// re-enter.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// String returns the stable phase name.
func (p Phase) String() string {
	switch p {
	case PhaseCopying:
		return "copying"
	case PhaseCatchingUp:
		return "catching_up"
	case PhaseVerifying:
		return "verifying"
	case PhaseCutover:
		return "cutover"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	default:
		return fmt.Sprintf("Phase(%d)", p)
	}
}

// Checkpoint is the single durable resume record.
type Checkpoint struct {
	Schema            string
	Table             string
	ShadowTable       string
	SlotName          string
	PublicationName   string
	Watermark         copier.Watermark
	LastAppliedLSN    decode.LSN
	SourceFingerprint string
	TargetFingerprint string
	Phase             Phase
	UpdatedAt         time.Time
}
