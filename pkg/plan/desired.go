package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseDestructive marks the whole desired-state preview non-executable when
// any step is destructive. It preserves existing refusals, withdraws execution metadata, and
// fingerprints the resulting plan. Execution still enforces its own admission.
func RefuseDestructive(report *Report) {
	destructive := false
	for _, st := range report.Statements {
		destructive = destructive || st.Destructive
	}
	if !destructive {
		return
	}
	refuseStatements(report, func(_ int) (verdict.Refusal, executor.CreateShapeCause) {
		return DestructiveChangeRefusal(), ""
	})
	// Whole-plan admission checks destructiveness before routed dispositions.
	// Preserve each statement's specific refusal, but give the aggregate the same
	// precedence as apply, including when routing already refused the report.
	r := DestructiveChangeRefusal()
	report.Reason, report.Class, report.Owner = r.Reason(), r.Class(), r.Owner()
	report.Fingerprint = Fingerprint(report.Statements)
}
