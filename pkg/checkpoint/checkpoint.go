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

// parsePhase reads a stable phase name back into its Phase. A name String
// never produces is an error: the store wrote the row, so an unknown name
// is a row the store cannot have written.
func parsePhase(name string) (Phase, error) {
	for p := PhaseCopying; p <= PhaseFailed; p++ {
		if p.String() == name {
			return p, nil
		}
	}
	return 0, fmt.Errorf("unknown phase %q", name)
}

// Fingerprints identifies the statement a checkpoint belongs to: the digests
// of the source's and the shadow's introspected models. Two runs with the
// same pair are the same schema change; a row carrying another pair is
// another statement's state (ST-2).
type Fingerprints struct {
	// Source is the digest of the source table's introspected model.
	Source string
	// Target is the digest of the shadow's introspected model after the
	// statement ran on it: the after-schema, not the SQL text.
	Target string
}

// Identity is everything a stored row is guarded on: the row format it was
// written in and the statement it belongs to. Save refuses to write over a
// row with another Identity, and Delete removes a row only when its
// Identity is the one the caller was shown.
type Identity struct {
	FormatVersion int32
	Fingerprints  Fingerprints
}

// Checkpoint is the single durable resume record.
type Checkpoint struct {
	Schema          string
	Table           string
	ShadowTable     string
	SlotName        string
	PublicationName string
	// Watermark is the copier's landed frontier; its zero value means
	// nothing has landed yet.
	Watermark      copier.Watermark
	LastAppliedLSN decode.LSN
	// SourceFingerprint and TargetFingerprint are the Fingerprints the row
	// is keyed on.
	SourceFingerprint string
	TargetFingerprint string
	Phase             Phase
	// UpdatedAt is when the record was last saved, from the store's clock.
	UpdatedAt time.Time
}

// Fingerprints returns the statement identity the checkpoint carries.
func (c Checkpoint) Fingerprints() Fingerprints {
	return Fingerprints{Source: c.SourceFingerprint, Target: c.TargetFingerprint}
}

// Identity returns the row identity a Save of this checkpoint writes: the
// current FormatVersion and the checkpoint's fingerprints. A run that wants
// to discard its own row hands it to Delete.
func (c Checkpoint) Identity() Identity {
	return Identity{FormatVersion: FormatVersion, Fingerprints: c.Fingerprints()}
}

// validate refuses a checkpoint the store must not persist: the key, the
// fingerprints, and the phase are what resume keys on, so none may be empty
// or unknown. The slot and publication names may be empty — a quiesced run
// decodes no WAL and has neither.
func (c Checkpoint) validate() error {
	for _, field := range []struct{ name, value string }{
		{"schema", c.Schema},
		{"table", c.Table},
		{"shadow table", c.ShadowTable},
		{"source fingerprint", c.SourceFingerprint},
		{"target fingerprint", c.TargetFingerprint},
	} {
		if field.value == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidCheckpoint, field.name)
		}
	}
	if _, err := parsePhase(c.Phase.String()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCheckpoint, err)
	}
	return nil
}
