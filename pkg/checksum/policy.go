package checksum

import (
	"errors"
	"fmt"
)

// DivergencePolicy says what a pass does with a chunk whose source and
// shadow differ. It is a setting the caller states for every pass, never a
// default and never inferred from what happens to be wired up (CO-3): in
// the steady state a divergence is a defect and aborts; only a pass that
// expects differences — reconciliation after a lost replication slot —
// repairs them. The zero value names no policy and is refused.
type DivergencePolicy string

const (
	// DivergenceAbort ends the pass at its report: nothing is written, and
	// the caller refuses to cut over.
	DivergenceAbort DivergencePolicy = "abort"
	// DivergenceRepair recopies every differing chunk from the source and
	// reads it again; the pass still mints no proof.
	DivergenceRepair DivergencePolicy = "repair"
)

// ErrNoDivergencePolicy reports a pass asked to run without saying what a
// difference means.
var ErrNoDivergencePolicy = errors.New("no divergence policy")

// ParseDivergencePolicy returns the policy a configuration value or flag
// names, or ErrNoDivergencePolicy when it names neither, so a caller can
// refuse a setting when it loads it rather than at its first pass.
func ParseDivergencePolicy(value string) (DivergencePolicy, error) {
	policy := DivergencePolicy(value)
	if err := policy.validate(); err != nil {
		return "", err
	}
	return policy, nil
}

// validate refuses every value but the two named policies.
func (p DivergencePolicy) validate() error {
	// INV: CO-3
	switch p {
	case DivergenceAbort, DivergenceRepair:
		return nil
	case "":
		return fmt.Errorf("%w: a pass must state %q or %q", ErrNoDivergencePolicy, DivergenceAbort, DivergenceRepair)
	default:
		return fmt.Errorf("%w: %q is not %q or %q", ErrNoDivergencePolicy, string(p), DivergenceAbort, DivergenceRepair)
	}
}

// DivergenceError is a pass that found differences under DivergenceAbort.
// It carries the report so the caller can say which chunks differed; the
// shadow is as the pass found it.
type DivergenceError struct {
	// Report is the pass that found the differences.
	Report Report
}

func (e *DivergenceError) Error() string {
	return fmt.Sprintf("source and shadow differ in %d of %d chunks through watermark %d", len(e.Report.Mismatches), e.Report.Chunks, e.Report.Through.Value())
}

// RepairError is a chunk that still differed when read again after its
// repair. A recopy that does not converge means something other than the
// copier writes the shadow, or the source changed between the recopy and
// the read, which a repair pass assumes does not happen; either way the
// pass cannot vouch for the chunk, and repairing it again would not say
// why. The recopy has committed, with every other chunk's: the Outcome
// returned alongside lists them.
type RepairError struct {
	// Repair is the recopy that did not take.
	Repair Repair
	// After is the chunk as the fresh read found it.
	After Mismatch
}

func (e *RepairError) Error() string {
	return fmt.Sprintf("chunk [%d, %d] still differs after its repair: source %d rows, shadow %d rows", e.After.Chunk.Lower(), e.After.Chunk.Upper(), e.After.Source.Rows, e.After.Shadow.Rows)
}
