package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/verdict"
)

// keyTableDesired is the constraint fixture's table without its unique
// constraint, followed by index.
func keyTableDesired(index string) string {
	return `CREATE TABLE t (
		id int PRIMARY KEY,
		email text,
		name text,
		qty int NOT NULL,
		CONSTRAINT t_qty_check CHECK (qty > 0)
	);
	` + index + `;`
}

// A unique index the file renames, and changes in no other way, is renamed
// in the catalog: the plan is one ALTER INDEX ... RENAME TO, so the index
// never stops rejecting duplicates. The re-plan is empty.
func TestRunDesiredRenamesAUniqueIndexInPlace(t *testing.T) {
	pool, schema := replaceConstraintTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE UNIQUE INDEX t_name_uq ON %s.t (lower(name))`, schema))
	require.NoError(t, err)
	desired := replaceDesired("UNIQUE (email, name)", "CHECK (qty > 0)") + `
	CREATE UNIQUE INDEX t_name_lower_uq ON t (lower(name));`

	planned := plannedDrop(t, pool, schema, desired)
	require.Len(t, planned.Statements, 1)
	assert.Equal(t, fmt.Sprintf(`ALTER INDEX %s.t_name_uq RENAME TO t_name_lower_uq`, schema), planned.Statements[0].SQL)

	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	def, valid := indexState(t, pool, schema, "t_name_lower_uq")
	assert.Contains(t, def, "CREATE UNIQUE INDEX t_name_lower_uq")
	assert.True(t, valid)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_name_uq")
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements)
}

// A unique constraint the file declares again as a unique index is replaced
// before it goes: the plan builds the index, then drops the constraint, so
// duplicates are rejected throughout. When the rows break the new index's
// rule, the build fails and the constraint still stands; when they fit, the
// constraint is dropped once the index is valid.
func TestRunDesiredDropsAUniqueConstraintOnlyAfterItsIndexIsBuilt(t *testing.T) {
	t.Run("the build fails and the constraint stays", func(t *testing.T) {
		pool, schema := replaceConstraintTable(t)
		desired := keyTableDesired("CREATE UNIQUE INDEX t_email_uq ON t (email)")
		planned := plannedDrop(t, pool, schema, desired)
		require.Len(t, planned.Statements, 2)
		assert.Contains(t, planned.Statements[0].SQL, "CREATE UNIQUE INDEX t_email_uq")
		assert.Contains(t, planned.Statements[1].SQL, "DROP CONSTRAINT t_email_key")

		err := runReplaceFails(t, pool, schema, desired)
		assert.Contains(t, err.Error(), "23505", "both rows have the same email")
		def, validated := constraintDef(t, pool, schema, "t_email_key")
		assert.Equal(t, "UNIQUE (email, name)", def, "the old uniqueness still holds")
		assert.True(t, validated)
	})

	t.Run("the build succeeds and the constraint goes", func(t *testing.T) {
		pool, schema := replaceConstraintTable(t)
		desired := keyTableDesired("CREATE UNIQUE INDEX t_email_name_uq ON t (email, name)")
		res := runReplace(t, pool, schema, desired)
		require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
		_, valid := indexState(t, pool, schema, "t_email_name_uq")
		assert.True(t, valid)
		assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_key")
		assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements)
	})
}
