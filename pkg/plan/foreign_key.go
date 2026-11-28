package plan

import (
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// RefuseUndeclarableForeignKeyDrops marks as refused every statement that
// drops one of the named foreign keys: a live key a desired file cannot
// declare, such as one into another schema, so the diff plans its drop
// whatever the file says. Running it would remove a key the file's author had no way to keep.
// Every other foreign-key drop keeps its route and runs as a destructive,
// checked DROP CONSTRAINT. The rest of the plan keeps its routing; admission
// is all-or-nothing, so one refused statement keeps the whole plan from
// running.
func RefuseUndeclarableForeignKeyDrops(report *Report, undeclarable map[string]bool) {
	refuseForeignKeyDrops(report, undeclarable)
}

// RefuseChangedForeignKeyDrops marks as refused every statement that drops
// one of the named foreign keys: a live key the plan drops before the
// foreign key that replaces it is added, such as one the desired file
// changes under the same name on a partitioned table, which cannot take the
// in-place replacement. Between the two statements writes are checked
// against no key, so rows the new key forbids could get in. Declaring the
// changed key under a new name adds it before the old one is dropped.
func RefuseChangedForeignKeyDrops(report *Report, changed map[string]bool) {
	refuseForeignKeyDrops(report, changed)
}

// refuseForeignKeyDrops refuses each statement that drops one of the named
// foreign keys with declarative-foreign-key.
func refuseForeignKeyDrops(report *Report, keys map[string]bool) {
	refuseStatements(report, func(i int) (verdict.Refusal, executor.CreateShapeCause) {
		st := report.Statements[i]
		if st.Kind != schemadiff.ChangeDropForeignKey || st.Drops == nil || !keys[st.Drops.Name] {
			return verdict.Refusal{}, ""
		}
		return ForeignKeyChangeRefusal(), ""
	})
}
