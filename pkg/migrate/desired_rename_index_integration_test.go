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

// A file removes the unique index t_email_uq on email and makes the plain
// index t_email_lookup on lower(email) unique, which rows 1 and 3 break. The
// plan builds the new unique index before it drops t_email_uq, so the failed
// build leaves t_email_uq rejecting duplicates. Once the rows fit and the
// failed build is cleared, the next run swaps t_email_lookup for its unique
// form, drops t_email_uq, and a re-plan is empty.
func TestRunDesiredKeepsAUniqueIndexWhileAnotherBecomesUnique(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE INDEX t_email_lookup ON %[1]s.t (lower(email));
		CREATE UNIQUE INDEX t_email_uq ON %[1]s.t (email)`, schema))
	require.NoError(t, err)
	desired := `CREATE TABLE t (
		id int PRIMARY KEY,
		email text,
		name text,
		qty int NOT NULL,
		deleted_at timestamptz
	);
	CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL;
	CREATE UNIQUE INDEX t_email_lookup ON t (lower(email));`

	planned := plannedDrop(t, pool, schema, desired)
	require.NotEmpty(t, planned.Statements)
	assert.Contains(t, planned.Statements[len(planned.Statements)-1].SQL, "t_email_uq", "the old uniqueness is dropped last")

	err = runReplaceFails(t, pool, schema, desired)
	assert.Contains(t, err.Error(), "t_email_lookup_new", "the failure names the build it left behind")
	def, valid := indexState(t, pool, schema, "t_email_uq")
	assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX t_email_uq ON %s.t USING btree (email)", schema), def, "the old uniqueness still holds")
	assert.True(t, valid)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (4, 'b@example.com', 'd', 1, NULL)`, schema))
	require.Error(t, err, "the old unique index still rejects a duplicate")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %[1]s.t WHERE id = 3; DROP INDEX %[1]s.t_email_lookup_new`, schema))
	require.NoError(t, err)
	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_uq")
	def, valid = indexState(t, pool, schema, "t_email_lookup")
	assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX t_email_lookup ON %s.t USING btree (lower(email))", schema), def)
	assert.True(t, valid)
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements, "the change converges")
}
