package preflight

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/statement"
)

// RequiredTier derives the engine-role tier a routed change's exec SQL
// needs: an in-place ALTER TABLE step is owner-gated (TierAlterInPlace),
// and any step that builds a new index — every CREATE INDEX, and the ALTER
// TABLE shapes that build one as a side effect — additionally needs CREATE
// on the schema (TierIndexBuild), so the requirement is the most demanding
// step's tier — the ladder check covers every rung below it. The mapping
// from step shape to required access lives here, next to Tier and
// CheckPrivileges, so every consumer of the routed plan derives the same
// answer. A step shape the engine does not execute fails closed here,
// before anything runs.
func RequiredTier(execSQL []string) (Tier, error) {
	tier := TierAlterInPlace
	for _, sql := range execSQL {
		st, err := statement.ParseOne(sql)
		if err != nil {
			return 0, fmt.Errorf("derive privilege tier: %w", err)
		}
		switch st.Kind() {
		case statement.KindAlterTable, statement.KindCreateIndex:
			if st.BuildsIndex() {
				tier = TierIndexBuild
			}
		default:
			return 0, fmt.Errorf("derive privilege tier for step %q: kind %s is not a shape the engine executes",
				sql, st.Kind())
		}
	}
	return tier, nil
}
