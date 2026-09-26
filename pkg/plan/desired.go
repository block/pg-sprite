package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseDestructive marks destructive steps as non-executable for desired-state
// previews. It preserves existing refusals, withdraws execution metadata, and
// fingerprints the resulting plan. Execution still enforces its own admission.
func RefuseDestructive(report *Report) {
	refuseStatements(report, func(i int) (verdict.Refusal, executor.CreateShapeCause) {
		if report.Statements[i].Destructive {
			return verdict.ByDesign(verdict.ReasonDestructiveChange), ""
		}
		return verdict.Refusal{}, ""
	})
	report.Fingerprint = Fingerprint(report.Statements)
}
