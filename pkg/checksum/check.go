package checksum

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/copier"
)

// Outcome is one pass run under a divergence policy: what it compared and
// found, what it repaired, and — only when it found nothing and repaired
// nothing — the proofs the cutover and the checkpoint demand. A pass that
// repaired a chunk has read that chunk clean afterwards, but it mints no
// proof: the proof comes from the next pass, which reads every chunk
// fresh (CO-2).
type Outcome struct {
	// Report is the comparison the pass made before any repair.
	Report Report
	// Repairs lists the chunks the pass recopied under DivergenceRepair, in
	// ascending key order. It is empty under DivergenceAbort and for a
	// clean pass.
	Repairs []Repair

	clean    CleanWatermark
	verified VerifiedShadow
}

// Clean reports whether the pass found every compared chunk equal, so it
// repaired nothing.
func (o Outcome) Clean() bool { return o.Report.Clean() }

// CleanWatermark returns the proof that every chunk through the pass's
// watermark was read clean, and whether the pass minted one.
func (o Outcome) CleanWatermark() (CleanWatermark, bool) {
	return o.clean, o.clean.Watermark().Valid()
}

// VerifiedShadow returns the proof that the whole shadow equals its source,
// and whether the pass minted one: a clean pass through the complete
// watermark does; a clean pass through a partial watermark proves only
// its prefix and does not.
func (o Outcome) VerifiedShadow() (VerifiedShadow, bool) {
	return o.verified, o.verified.Table() != ""
}

// Check runs one verification pass through the copier's landed watermark
// and acts on what it finds as policy says. A clean pass mints a
// CleanWatermark, and a VerifiedShadow when the watermark is complete.
// Under DivergenceAbort a difference is returned as a *DivergenceError
// carrying the report, with the shadow untouched. Under DivergenceRepair
// every differing chunk is recopied from the source in one transaction and
// read again in a fresh snapshot; a chunk still different after its repair
// is returned as a *RepairError. A pass whose repairs all took returns a nil
// error and no proof. The policy is stated for every pass; there is no
// default (CO-3).
func (v *Verifier) Check(ctx context.Context, pool *pgxpool.Pool, through copier.Watermark, policy DivergencePolicy) (Outcome, error) {
	if err := policy.validate(); err != nil {
		return Outcome{}, fmt.Errorf("verify %s.%s: %w", v.target.Schema(), v.target.Table(), err)
	}
	report, err := v.Verify(ctx, pool, through)
	if err != nil {
		return Outcome{}, err
	}
	if report.Clean() {
		return v.prove(report), nil
	}
	// INV: CO-3
	switch policy {
	case DivergenceAbort:
		return Outcome{}, &DivergenceError{Report: report}
	case DivergenceRepair:
		repairs, err := v.repairAll(ctx, pool, report.Mismatches)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Report: report, Repairs: repairs}, nil
	default:
		return Outcome{}, fmt.Errorf("%w (CO-3): divergence policy %q passed validation", ErrInvariantViolation, string(policy))
	}
}

// prove mints the proofs a clean pass earns: the clean watermark always,
// and the verified shadow only when nothing lies beyond the watermark.
func (v *Verifier) prove(report Report) Outcome {
	// INV: CO-1, CO-2
	outcome := Outcome{Report: report, clean: newCleanWatermark(report.Through)}
	if report.Through.Complete() {
		outcome.verified = newVerifiedShadow(v.shadow, report.Through, v.opts.Clock.Now())
	}
	return outcome
}
