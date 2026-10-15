package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseForeignKeyChanges marks every statement that adds or drops a foreign
// key as refused. Desired files may declare foreign keys so a plan can show
// whether the live table matches them, but desired-state execution does not
// run foreign-key DDL yet: the plan reports the change and nothing executes
// it. The rest of the plan keeps its routing; admission is all-or-nothing, so
// one refused statement keeps the whole plan from running.
func RefuseForeignKeyChanges(report *Report) {
	refuseStatements(report, func(i int) (verdict.Refusal, executor.CreateShapeCause) {
		if !report.Statements[i].Kind.IsForeignKey() {
			return verdict.Refusal{}, ""
		}
		return ForeignKeyChangeRefusal(), ""
	})
}
