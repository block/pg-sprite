package plan

import (
	"slices"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseDestructive marks the whole desired-state preview non-executable when
// any step is destructive. It preserves existing refusals, withdraws execution metadata, and
// fingerprints the resulting plan. The identity the plan had before is kept in
// PinnableFingerprint, since it is the one a run pins to execute the plan; a
// report refused again keeps the identity it was first given. The caller's
// statements and decisions are copied before they change, so a report passed
// by value leaves the caller's copy as it was. Execution still enforces its
// own admission.
func RefuseDestructive(report *Report) {
	destructive := false
	for _, st := range report.Statements {
		destructive = destructive || st.Destructive
	}
	if !destructive {
		return
	}
	report.Statements = slices.Clone(report.Statements)
	for i := range report.Statements {
		report.Statements[i].Decisions = slices.Clone(report.Statements[i].Decisions)
	}
	refuseStatements(report, func(_ int) (verdict.Refusal, executor.CreateShapeCause) {
		return DestructiveChangeRefusal(), ""
	})
	// Whole-plan admission checks destructiveness before routed dispositions.
	// Preserve each statement's specific refusal, but give the aggregate the same
	// precedence as apply, including when routing already refused the report.
	r := DestructiveChangeRefusal()
	report.Reason, report.Class, report.Owner = r.Reason(), r.Class(), r.Owner()
	if report.PinnableFingerprint == "" {
		report.PinnableFingerprint = report.Fingerprint
	}
	report.Fingerprint = report.ComputeFingerprint()
}
