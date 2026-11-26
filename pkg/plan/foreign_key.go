package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseUndeclarableForeignKeyDrops marks as refused every statement that
// drops one of the named foreign keys: a live key into another schema, which
// a desired file cannot declare, so the diff plans its drop whatever the file
// says. Running it would remove a key the file's author had no way to keep.
// Every other foreign-key drop keeps its route and runs as a destructive,
// checked DROP CONSTRAINT. The rest of the plan keeps its routing; admission
// is all-or-nothing, so one refused statement keeps the whole plan from
// running.
func RefuseUndeclarableForeignKeyDrops(report *Report, intoOtherSchemas map[string]bool) {
	refuseStatements(report, func(i int) (verdict.Refusal, executor.CreateShapeCause) {
		st := report.Statements[i]
		if st.Kind != schemadiff.ChangeDropForeignKey || st.Drops == nil || !intoOtherSchemas[st.Drops.Name] {
			return verdict.Refusal{}, ""
		}
		return ForeignKeyChangeRefusal(), ""
	})
}
