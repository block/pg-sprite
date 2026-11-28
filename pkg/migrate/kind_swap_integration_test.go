package migrate_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// liveConstraint returns the definition of the named constraint on schema.t,
// and whether it exists.
func liveConstraint(t *testing.T, pool *pgxpool.Pool, schema, name string) (string, bool) {
	t.Helper()
	var def string
	err := pool.QueryRow(t.Context(), `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = format('%I.t', $1::text)::regclass AND conname = $2`, schema, name).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	require.NoError(t, err, "constraint %s", name)
	return def, true
}

// A file turns the unique constraint t_email_key into a standalone unique
// index of the same name over lower(email), which rows 1 and 3 break. The
// plan builds t_email_key_new first, so the failed build leaves the
// constraint enforcing its rule, and a duplicate is still rejected. Once
// the rows fit and the failed build is cleared, the next run drops the
// constraint and renames the new index to its name, and a re-plan is empty.
func TestRunDesiredSwapsAUniqueConstraintForAUniqueIndexBuildingFirst(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.t ADD CONSTRAINT t_email_key UNIQUE (email)`, schema))
	require.NoError(t, err)
	const desired = `
CREATE TABLE t (
    id int PRIMARY KEY,
    email text,
    name text,
    qty int NOT NULL,
    deleted_at timestamptz
);
CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX t_email_key ON t (lower(email));`

	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 3)
	assert.Equal(t, []schemadiff.ChangeKind{schemadiff.ChangeCreateIndex, schemadiff.ChangeDropConstraint, schemadiff.ChangeRenameIndex},
		[]schemadiff.ChangeKind{reviewed.Statements[0].Kind, reviewed.Statements[1].Kind, reviewed.Statements[2].Kind})
	assert.Equal(t, "t_email_key_new", reviewed.Statements[1].ReplacedBy)

	err = runReplaceFails(t, pool, schema, desired)
	assert.Contains(t, err.Error(), "t_email_key_new", "the failure names the build it left behind")
	def, ok := liveConstraint(t, pool, schema, "t_email_key")
	require.True(t, ok, "the constraint is still in place")
	assert.Equal(t, "UNIQUE (email)", def)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (4, 'b@example.com', 'd', 1, NULL)`, schema))
	require.Error(t, err, "the constraint still rejects a duplicate")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %[1]s.t WHERE id = 3; DROP INDEX %[1]s.t_email_key_new`, schema))
	require.NoError(t, err)
	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	_, ok = liveConstraint(t, pool, schema, "t_email_key")
	assert.False(t, ok, "the constraint is gone")
	idx, valid := indexState(t, pool, schema, "t_email_key")
	assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX t_email_key ON %s.t USING btree (lower(email))", schema), idx)
	assert.True(t, valid)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_key_new")
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements, "the swap converges")
}

// A file turns the unique index t_qty_key over (qty, name) into a unique
// constraint of the same name over qty, which rows 1 and 4 break. The plan
// builds the constraint's index as t_qty_key_new first, so the failed build
// leaves the old index rejecting duplicates. Once the rows fit and the
// failed build is cleared, the next run drops the old index and the
// constraint takes over the new one under its name, and a re-plan is empty.
func TestRunDesiredSwapsAUniqueIndexForAUniqueConstraintBuildingFirst(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE UNIQUE INDEX t_qty_key ON %[1]s.t (qty, name);
		INSERT INTO %[1]s.t VALUES (4, 'd@example.com', 'd', 3, NULL)`, schema))
	require.NoError(t, err)
	const desired = `
CREATE TABLE t (
    id int PRIMARY KEY,
    email text,
    name text,
    qty int NOT NULL,
    deleted_at timestamptz,
    CONSTRAINT t_qty_key UNIQUE (qty)
);
CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL;`

	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 3)
	assert.Equal(t, []schemadiff.ChangeKind{schemadiff.ChangeCreateIndex, schemadiff.ChangeDropIndex, schemadiff.ChangeAddConstraint},
		[]schemadiff.ChangeKind{reviewed.Statements[0].Kind, reviewed.Statements[1].Kind, reviewed.Statements[2].Kind})
	assert.Equal(t, "t_qty_key_new", reviewed.Statements[1].ReplacedBy)

	err = runReplaceFails(t, pool, schema, desired)
	assert.Contains(t, err.Error(), "t_qty_key_new", "the failure names the build it left behind")
	idx, valid := indexState(t, pool, schema, "t_qty_key")
	assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX t_qty_key ON %s.t USING btree (qty, name)", schema), idx, "the old index is untouched")
	assert.True(t, valid)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (5, 'e@example.com', 'a', 3, NULL)`, schema))
	require.Error(t, err, "the old index still rejects a duplicate")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %[1]s.t WHERE id = 4; DROP INDEX %[1]s.t_qty_key_new`, schema))
	require.NoError(t, err)
	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	def, ok := liveConstraint(t, pool, schema, "t_qty_key")
	require.True(t, ok)
	assert.Equal(t, "UNIQUE (qty)", def)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_qty_key_new", "the constraint takes the built index under its name")
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements, "the swap converges")
}
