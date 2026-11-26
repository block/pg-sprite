package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseForeignKeyDrops marks every statement that drops a foreign key as
// refused. A foreign key the desired file adds or validates keeps its route:
// the planner runs an add as ADD CONSTRAINT ... NOT VALID then VALIDATE
// CONSTRAINT, so neither step holds a lock that stops reads or writes for
// longer than the brief budget. Desired-state execution does not drop a
// foreign key yet: the plan reports the drop and nothing executes it. The
// rest of the plan keeps its routing; admission is all-or-nothing, so one
// refused statement keeps the whole plan from running.
func RefuseForeignKeyDrops(report *Report) {
	refuseStatements(report, func(i int) (verdict.Refusal, executor.CreateShapeCause) {
		if report.Statements[i].Kind != schemadiff.ChangeDropForeignKey {
			return verdict.Refusal{}, ""
		}
		return ForeignKeyChangeRefusal(), ""
	})
}
