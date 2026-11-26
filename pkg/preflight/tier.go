package preflight

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// RequiredTier derives the engine-role tier a routed change's exec SQL
// needs: an in-place ALTER TABLE step, an ALTER INDEX ... RENAME TO on the
// table's index, and a DROP INDEX of that one index, is owner-gated
// (TierAlterInPlace),
// and any step that builds a new index — every CREATE INDEX, and the ALTER
// TABLE shapes that build one as a side effect — additionally needs CREATE
// on the schema (TierIndexBuild), so the requirement is the most demanding
// step's tier — the ladder check covers every rung below it. The mapping
// from step shape to required access lives here, next to Tier and
// CheckPrivileges, so every consumer of the routed plan derives the same
// answer. A step shape the engine does not execute fails closed here,
// before anything runs.
//
// A CREATE TABLE step derives the off-ladder TierCreateTable — the
// greenfield create plan's shape: one CREATE TABLE plus CREATE INDEX steps
// on the table it creates, checked by CheckCreatePrivileges, never by
// CheckPrivileges, whose ladder states facts about an existing table. A
// set that mixes CREATE TABLE with a change to an existing table fails
// closed: the off-ladder tier proves creation access only and cannot vouch
// for the ladder rungs a change to an existing table needs — and no front
// door produces such a set.
func RequiredTier(execSQL []string) (Tier, error) {
	tier := TierAlterInPlace
	var createsTable, changesExistingTable bool
	for _, sql := range execSQL {
		st, err := statement.ParseOne(sql)
		if err != nil {
			return 0, fmt.Errorf("derive privilege tier: %w", err)
		}
		switch st.Kind() {
		case statement.KindCreateTable:
			createsTable = true
		case statement.KindAlterTable:
			changesExistingTable = true
			if st.BuildsIndex() {
				tier = TierIndexBuild
			}
		case statement.KindCreateIndex:
			if st.BuildsIndex() {
				tier = TierIndexBuild
			}
		case statement.KindRenameIndex, statement.KindDropIndex:
			// Renaming or dropping an index needs ownership of it, and an
			// index is owned by its table's owner: the in-place rung. That
			// holds for one index only. Several indexes, or a CASCADE that
			// reaches dependent objects, can act on other tables, which a
			// one-table privilege check cannot vouch for.
			if st.IndexTarget() != statement.IndexTargetSingleRelation {
				return 0, fmt.Errorf("derive privilege tier for step %q: an index statement that reaches beyond one index is not a shape the engine executes", sql)
			}
			changesExistingTable = true
		default:
			return 0, fmt.Errorf("derive privilege tier for step %q: kind %s is not a shape the engine executes",
				sql, st.Kind())
		}
	}
	if createsTable {
		if changesExistingTable {
			return 0, fmt.Errorf("derive privilege tier: the set mixes CREATE TABLE with a change to an existing table; " +
				"the off-ladder create tier cannot vouch for the ladder rungs a change to an existing table needs")
		}
		return TierCreateTable, nil
	}
	return tier, nil
}
