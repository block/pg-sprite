package migrate_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// replaceIndexFixture is a soft-delete table whose live rows have unique
// emails, case-insensitively. Row 3 is deleted and shares row 1's email, so
// a rule over every row, deleted or not, does not fit.
const replaceIndexFixture = `CREATE TABLE %[1]s.t (
	id int PRIMARY KEY,
	email text,
	name text,
	qty int NOT NULL,
	deleted_at timestamptz
);
CREATE UNIQUE INDEX t_email_lower_key ON %[1]s.t (lower(email)) WHERE deleted_at IS NULL;
INSERT INTO %[1]s.t VALUES (1, 'a@example.com', 'a', 3, NULL), (2, 'b@example.com', 'b', 7, NULL), (3, 'A@example.com', 'c', 1, now())`

func replaceIndexTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, schema := replaceConstraintTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %[1]s.t; `+replaceIndexFixture, schema))
	require.NoError(t, err)
	return pool, schema
}

func replaceIndexDesired(index string) string {
	return `CREATE TABLE t (
		id int PRIMARY KEY,
		email text,
		name text,
		qty int NOT NULL,
		deleted_at timestamptz
	);
	` + index + `;`
}

// indexState returns the live definition of the named index on schema.t and
// whether it is valid.
func indexState(t *testing.T, pool *pgxpool.Pool, schema, name string) (def string, valid bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `
		SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = format('%I.t', $1::text)::regclass AND c.relname = $2`, schema, name).Scan(&def, &valid)
	require.NoError(t, err, "index %s", name)
	return def, valid
}

// A unique index whose predicate changes is replaced: the new index is built
// as t_email_lower_key_new while the old one keeps rejecting duplicates, the
// old one is dropped, and the new one takes its name. The drop names the
// index that replaces it, nothing is left under the temporary name, and a
// re-plan is empty.
func TestRunDesiredReplacesAChangedUniqueIndex(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	desired := replaceIndexDesired("CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL AND qty > 0")

	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 3)
	assert.Equal(t, []schemadiff.ChangeKind{schemadiff.ChangeCreateIndex, schemadiff.ChangeDropIndex, schemadiff.ChangeRenameIndex},
		[]schemadiff.ChangeKind{reviewed.Statements[0].Kind, reviewed.Statements[1].Kind, reviewed.Statements[2].Kind})
	assert.Equal(t, "t_email_lower_key_new", reviewed.Statements[1].ReplacedBy)
	assert.False(t, reviewed.Statements[2].Destructive)

	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)

	def, valid := indexState(t, pool, schema, "t_email_lower_key")
	assert.Contains(t, def, "WHERE ((deleted_at IS NULL) AND (qty > 0))")
	assert.True(t, valid)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_lower_key_new", "the built index takes the old index's name")
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements, "the replaced index converges")
}

// A unique index whose new definition the rows break never leaves the table
// without the rule: the new index fails to build while the old one still
// rejects duplicates, and the old index is still there, valid, after. Once
// the rows fit and the failed build is cleared as the error says, the next
// run completes the replacement.
func TestRunDesiredKeepsTheOldUniqueIndexWhenItsReplacementFails(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	desired := replaceIndexDesired("CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email))")

	err := runReplaceFails(t, pool, schema, desired)
	assert.Contains(t, err.Error(), "t_email_lower_key_new", "the failure names the build it left behind")

	def, valid := indexState(t, pool, schema, "t_email_lower_key")
	assert.Contains(t, def, "WHERE (deleted_at IS NULL)", "the old index is untouched")
	assert.True(t, valid)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (4, 'B@example.com', 'd', 1, NULL)`, schema))
	require.Error(t, err, "the old index still rejects a duplicate")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.t WHERE id = 3`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DROP INDEX CONCURRENTLY %s.t_email_lower_key_new`, schema))
	require.NoError(t, err)
	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	def, valid = indexState(t, pool, schema, "t_email_lower_key")
	assert.NotContains(t, def, "WHERE")
	assert.True(t, valid)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_lower_key_new")
}

// A replacement that stopped after the old index was dropped is finished by
// the rename alone: the build under the temporary name, which already
// enforces the rule, is kept rather than dropped for a fresh build.
func TestRunDesiredFinishesAReplacementThatStoppedBeforeTheRename(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE UNIQUE INDEX t_email_lower_key_new ON %[1]s.t (lower(email)) WHERE deleted_at IS NULL AND qty > 0;
		DROP INDEX %[1]s.t_email_lower_key`, schema))
	require.NoError(t, err)
	desired := replaceIndexDesired("CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL AND qty > 0")

	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)
	assert.Equal(t, schemadiff.ChangeRenameIndex, reviewed.Statements[0].Kind)

	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	_, valid := indexState(t, pool, schema, "t_email_lower_key")
	assert.True(t, valid)
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements)
}

// A table over the size limit still has its changed index replaced: the
// build is an online idiom, the drop is concurrent, and the rename changes
// only the catalog, so none of the three is the blind bounded attempt the
// size guard exists for.
func TestRunDesiredReplacesAnIndexOnATableOverTheSizeLimit(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	desired := replaceIndexDesired("CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL AND qty > 0")
	reviewed := plannedDrop(t, pool, schema, desired)
	opts := runOptions()
	// A one-byte limit puts the table over it, as a large table would be.
	opts.MaxTableSizeBytes = 1

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, opts)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	def, valid := indexState(t, pool, schema, "t_email_lower_key")
	assert.Contains(t, def, "WHERE ((deleted_at IS NULL) AND (qty > 0))")
	assert.True(t, valid)
}

// A unique index that a foreign key on another table references cannot be
// dropped while the key remains, so a desired file that removes it or
// changes its definition is refused at plan time, naming the key. Nothing is
// built, and the index and the key stay as they were.
func TestRunDesiredRefusesDroppingAUniqueIndexAForeignKeyReferences(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE UNIQUE INDEX t_name_key ON %[1]s.t (name);
		CREATE TABLE %[1]s.r (id int PRIMARY KEY, name text CONSTRAINT r_name_fkey REFERENCES %[1]s.t (name))`, schema))
	require.NoError(t, err)
	const kept = "CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL"

	for name, tc := range map[string]struct {
		desired string
		keep    string
	}{
		"removed": {desired: replaceIndexDesired(kept), keep: "keep the index"},
		"changed": {desired: replaceIndexDesired(kept + ";\n\tCREATE UNIQUE INDEX t_name_key ON t (name, qty)"), keep: "keep the index's definition"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
				Schema: schema, Desired: parseDesired(t, tc.desired),
			}, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
			assert.Contains(t, res.Detail, `index "t_name_key" is referenced by foreign key "r_name_fkey" on table "`+schema+`"."r"`)
			assert.Contains(t, res.Detail, `drop or change "r_name_fkey" first, or `+tc.keep)

			def, valid := indexState(t, pool, schema, "t_name_key")
			assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX t_name_key ON %s.t USING btree (name)", schema), def)
			assert.True(t, valid)
			assert.NotContains(t, liveIndexes(t, pool, schema), "t_name_key_new", "nothing is built")
		})
	}
}

// A plan stored between its build and its drop, such as an apply that was
// stopped there, finds the old index's replacement as the plan built it
// before the old index goes. A replacement that was dropped, rebuilt with
// another definition, or left invalid in the meantime refuses the drop with
// replacement-index-drift, and the old index keeps enforcing its rule. The
// replacement as planned lets the drop run.
func TestReplacedIndexDropRefusesAReplacementThatChanged(t *testing.T) {
	pool, schema := replaceIndexTable(t)
	desired := replaceIndexDesired("CREATE UNIQUE INDEX t_email_lower_key ON t (lower(email)) WHERE deleted_at IS NULL AND qty > 0")
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 3)
	build, drop := reviewed.Statements[0], reviewed.Statements[1]
	require.NotNil(t, drop.Replacement)
	require.NotNil(t, drop.Drops)
	assert.Equal(t, "t_email_lower_key_new", drop.Replacement.Index)
	budget := executor.ConcurrentBudget{Overall: time.Minute}
	dropOld := func(t *testing.T) error {
		t.Helper()
		_, err := executor.ExecuteReplacedIndexDropConcurrently(t.Context(), pool, schema, "t", "t_email_lower_key", drop.Drops.Definition, *drop.Replacement, budget)
		return err
	}
	exec := func(t *testing.T, sql string) {
		t.Helper()
		_, err := pool.Exec(t.Context(), sql)
		require.NoError(t, err)
	}
	requireOldIndexKept := func(t *testing.T, err error) {
		t.Helper()
		require.ErrorIs(t, err, executor.ErrReplacementIndexDrift)
		assert.Equal(t, executor.CodeReplacementIndexDrift, executor.OutcomeCode(err))
		def, valid := indexState(t, pool, schema, "t_email_lower_key")
		assert.Contains(t, def, "WHERE (deleted_at IS NULL)", "the old index is untouched")
		assert.True(t, valid)
	}
	temp := fmt.Sprintf("%s.t_email_lower_key_new", schema)

	requireOldIndexKept(t, dropOld(t))

	exec(t, fmt.Sprintf(`CREATE UNIQUE INDEX t_email_lower_key_new ON %s.t (lower(email)) WHERE deleted_at IS NULL`, schema))
	err := dropOld(t)
	requireOldIndexKept(t, err)
	assert.Contains(t, err.Error(), "not the", "the refusal names the definition it found")

	exec(t, "DROP INDEX "+temp)
	leaveInvalidBuild(t, pool, schema, build.SQL)
	requireOldIndexKept(t, dropOld(t))

	exec(t, "DROP INDEX "+temp)
	exec(t, build.SQL)
	require.NoError(t, dropOld(t))
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_lower_key")
	assert.Contains(t, liveIndexes(t, pool, schema), "t_email_lower_key_new")
}

// leaveInvalidBuild leaves the index build sql creates on schema.t invalid,
// the way an interrupted CREATE INDEX CONCURRENTLY does: an open transaction
// that wrote to the table holds the build in its wait for writers, and the
// build's statement timeout cancels it after its catalog entry committed. It
// writes no catalog row, which a managed server's role cannot.
func leaveInvalidBuild(t *testing.T, pool *pgxpool.Pool, schema, sql string) {
	t.Helper()
	writer, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.Rollback(t.Context())) }()
	_, err = writer.Exec(t.Context(), fmt.Sprintf(`UPDATE %s.t SET qty = qty WHERE id = 1`, schema))
	require.NoError(t, err)

	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(t.Context(), `SET statement_timeout = '500ms'`)
	require.NoError(t, err)
	defer func() {
		_, err := conn.Exec(t.Context(), `RESET statement_timeout`)
		require.NoError(t, err)
	}()
	_, err = conn.Exec(t.Context(), strings.Replace(sql, "CREATE UNIQUE INDEX ", "CREATE UNIQUE INDEX CONCURRENTLY ", 1))
	require.Error(t, err, "the build waits on the open writer until its timeout")
	def, valid := indexState(t, pool, schema, "t_email_lower_key_new")
	require.NotEmpty(t, def, "the cancelled build leaves its catalog entry")
	require.False(t, valid)
}
