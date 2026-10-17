package checkpoint

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// Phase identifies the durable schema-change phase. Values are opaque
// labels: the happy path visits them in declaration order, but PhaseFailed
// is reachable from any phase, so callers never compare phases for order —
// they switch on the value or ask Terminal.
//
// The set of phases is append-only within a row format: a new phase is
// added without a FormatVersion bump, since every row in a phase this
// engine knows still reads the same way. The one row an older engine of
// the same format cannot read is a row left in a phase it does not know,
// and Load and Inspect report that row as an IncompatibleError on
// MismatchPhase — written by a newer engine — carrying the row's Identity,
// not as a row no Save could have written. Removing or renaming a phase
// changes what stored rows mean and is a FormatVersion bump.
type Phase uint8

const (
	// PhaseCopying copies source rows.
	PhaseCopying Phase = iota + 1
	// PhaseCatchingUp applies captured changes.
	PhaseCatchingUp
	// PhaseReconciling recovers from slot loss (ST-4): the shadow and the
	// watermark are kept, the row's slot is not to be trusted, and a new
	// slot and a recopy pass — comparing every landed chunk and recopying
	// the ones that differ, without reading them again — must both
	// complete before change capture resumes. A resume that finds this
	// phase reconciles again from the top, whatever the slot looks like.
	PhaseReconciling
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
	case PhaseReconciling:
		return "reconciling"
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
// never produces is an error: this engine has no phase of that name.
func parsePhase(name string) (Phase, error) {
	for p := PhaseCopying; p <= PhaseFailed; p++ {
		if p.String() == name {
			return p, nil
		}
	}
	return 0, fmt.Errorf("unknown phase %q", name)
}

// knownPhaseNames lists the stable names of every phase this engine knows,
// in declaration order; it is what an IncompatibleError on MismatchPhase
// reports as this engine's side.
func knownPhaseNames() string {
	names := make([]string, 0, PhaseFailed)
	for p := PhaseCopying; p <= PhaseFailed; p++ {
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
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
	// ShadowProof is the JSON proof of the shadow the run built, as the
	// builder rendered it, so a resume after the swap — when the shadow
	// no longer bears its name and the catalog cannot re-mint the proof —
	// still knows which relations the swap was to move. It is empty for a
	// run that has no shadow to prove. It comes back as the server renders
	// JSON, not byte for byte as it was saved.
	//
	// Its lifecycle is the runner's, not the store's. The runner writes it
	// on the first Save after the build and decodes it on resume
	// (schemachange.Proof); it must be a JSON object, with no NUL character
	// in any string, or Save refuses it (ErrInvalidCheckpoint). The store
	// keeps it write-once: a later Save that carries no proof leaves the
	// row's in place, a later Save that carries the same proof is a no-op
	// on this column, and a later Save that carries a different proof is
	// refused (ErrInvariantViolation) — a different shadow is a fresh
	// start, which goes through Delete. Whether a row that should carry a
	// proof does, and whether the proof still describes the shadow, is the
	// runner's check, not the store's.
	ShadowProof json.RawMessage
	// Statement is the SQL text of the schema change the run is carrying
	// out, as the runner was given it, so a resume can refuse a statement
	// other than the one the row was written for even when the two leave
	// the same fingerprints — the fingerprints describe the shape the
	// change arrives at, not the operation that gets there. It is empty in
	// a row an engine that did not record the statement wrote.
	//
	// Its lifecycle is the runner's, like ShadowProof's: the runner sets it
	// on every Save and compares it on resume, and the store keeps it
	// write-once — a Save without one leaves the row's in place, a Save
	// with the same text is a no-op on this column, and a Save with other
	// text is refused (ErrInvariantViolation), because another statement is
	// a fresh start, which goes through Delete.
	Statement string
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
// or unknown, and a shadow proof, when there is one, must be a JSON
// object the column can hold. The slot and publication names may be empty — a
// quiesced run decodes no WAL and has neither.
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
	return validateShadowProof(c.ShadowProof)
}
