package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

const replaceConstraintFixture = `CREATE TABLE %[1]s.t (
	id int PRIMARY KEY,
	email text,
	name text,
	qty int NOT NULL,
	CONSTRAINT t_email_key UNIQUE (email, name),
	CONSTRAINT t_qty_check CHECK (qty > 0)
);
INSERT INTO %[1]s.t VALUES (1, 'a@example.com', 'a', 3), (2, 'a@example.com', 'b', 7)`

func replaceConstraintTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(replaceConstraintFixture, schema))
	require.NoError(t, err)
	return pool, schema
}

// constraintDef returns the live definition of the named constraint on
// schema.t, and whether it is validated.
func constraintDef(t *testing.T, pool *pgxpool.Pool, schema, name string) (def string, validated bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `
		SELECT pg_get_constraintdef(c.oid), c.convalidated
		FROM pg_constraint c
		WHERE c.conrelid = format('%I.t', $1::text)::regclass AND c.conname = $2`, schema, name).Scan(&def, &validated)
	require.NoError(t, err, "constraint %s", name)
	return def, validated
}

func runReplace(t *testing.T, pool *pgxpool.Pool, schema, desired string) migrate.DesiredResult {
	t.Helper()
	reviewed := plannedDrop(t, pool, schema, desired)
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, runOptions())
	require.NoError(t, err)
	return res
}

// runReplaceFails runs desired as runReplace does and returns the error
// the run fails with.
func runReplaceFails(t *testing.T, pool *pgxpool.Pool, schema, desired string) error {
	t.Helper()
	reviewed := plannedDrop(t, pool, schema, desired)
	_, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema, Desired: parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint, AllowDestructive: schema + ".t",
	}, runOptions())
	require.Error(t, err)
	return err
}

func replaceDesired(email, check string) string {
	return fmt.Sprintf(`CREATE TABLE t (
		id int PRIMARY KEY,
		email text,
		name text,
		qty int NOT NULL,
		CONSTRAINT t_email_key %s,
		CONSTRAINT t_qty_check %s
	);`, email, check)
}

// A unique constraint whose columns change converges in place: its new
// index is built beside the old one, and one statement swaps them. The
// constraint keeps its name, no temporary index is left behind, and a
// re-plan is empty.
func TestRunDesiredReplacesAUniqueConstraintInPlace(t *testing.T) {
	pool, schema := replaceConstraintTable(t)
	desired := replaceDesired("UNIQUE (email, name, qty)", "CHECK (qty > 0)")

	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)

	def, validated := constraintDef(t, pool, schema, "t_email_key")
	assert.Equal(t, "UNIQUE (email, name, qty)", def)
	assert.True(t, validated)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_key_new", "the built index takes the constraint's name")

	again := plannedDrop(t, pool, schema, desired)
	assert.Empty(t, again.Statements, "the replaced constraint converges")
}

// A unique constraint whose new definition the rows break never leaves the
// table without a rule: the new index fails to build while the old
// constraint still enforces, and the old constraint is still there after.
func TestRunDesiredKeepsTheOldUniqueConstraintWhenTheNewIndexFails(t *testing.T) {
	pool, schema := replaceConstraintTable(t)
	desired := replaceDesired("UNIQUE (email)", "CHECK (qty > 0)")

	err := runReplaceFails(t, pool, schema, desired)
	assert.Contains(t, err.Error(), "t_email_key_new", "the failure names the build it left to drop")

	def, validated := constraintDef(t, pool, schema, "t_email_key")
	assert.Equal(t, "UNIQUE (email, name)", def, "the old rule is untouched")
	assert.True(t, validated)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (3, 'a@example.com', 'a', 1)`, schema))
	require.Error(t, err, "the old rule still rejects a duplicate")
}

// A CHECK whose definition changes is swapped for the new rule NOT VALID in
// one statement, then validated. When existing rows break the new rule, the
// VALIDATE fails, but the new rule is already in place and checks every new
// write: at no point does the table accept a row neither rule allows.
func TestRunDesiredReplacesACheckWithoutAGap(t *testing.T) {
	pool, schema := replaceConstraintTable(t)

	t.Run("rows that pass the new rule", func(t *testing.T) {
		desired := replaceDesired("UNIQUE (email, name)", "CHECK (qty > 1)")
		res := runReplace(t, pool, schema, desired)
		require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
		def, validated := constraintDef(t, pool, schema, "t_qty_check")
		assert.Equal(t, "CHECK ((qty > 1))", def)
		assert.True(t, validated)
		assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements)
	})

	t.Run("a row that breaks the new rule", func(t *testing.T) {
		desired := replaceDesired("UNIQUE (email, name)", "CHECK (qty > 5)")
		err := runReplaceFails(t, pool, schema, desired)
		assert.Contains(t, err.Error(), "SQLSTATE 23514", "row 1 has qty 3")

		def, validated := constraintDef(t, pool, schema, "t_qty_check")
		assert.Equal(t, "CHECK ((qty > 5)) NOT VALID", def)
		assert.False(t, validated)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t VALUES (4, 'c@example.com', 'c', 4)`, schema))
		require.Error(t, err, "the new rule checks new writes before it is validated")
	})
}

// A unique constraint replacement that stopped after its build is picked up
// there: the valid t_email_key_new with the desired definition is swapped in,
// not built again and not dropped as an index the file does not declare. The
// constraint ends with the new definition, and a re-plan is empty.
func TestRunDesiredResumesAUniqueConstraintReplacementAfterItsBuild(t *testing.T) {
	pool, schema := replaceConstraintTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE UNIQUE INDEX t_email_key_new ON %s.t (email, name, qty)`, schema))
	require.NoError(t, err)
	desired := replaceDesired("UNIQUE (email, name, qty)", "CHECK (qty > 0)")

	planned := plannedDrop(t, pool, schema, desired)
	require.Len(t, planned.Statements, 1, "only the swap is left")
	assert.Contains(t, planned.Statements[0].SQL, "ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key_new")

	res := runReplace(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	def, validated := constraintDef(t, pool, schema, "t_email_key")
	assert.Equal(t, "UNIQUE (email, name, qty)", def)
	assert.True(t, validated)
	assert.NotContains(t, liveIndexes(t, pool, schema), "t_email_key_new")
	assert.Empty(t, plannedDrop(t, pool, schema, desired).Statements)
}

// A primary key or unique constraint that a foreign key on another table
// references cannot be dropped while the key remains, so a file that changes
// its definition is refused at plan time, naming the foreign key. Nothing is
// built, and the constraint stays as it was.
func TestRunDesiredRefusesReplacingAKeyAForeignKeyReferences(t *testing.T) {
	for name, tc := range map[string]struct {
		references string
		desired    string
		constraint string
	}{
		"a unique constraint": {
			references: "(email, name) REFERENCES %[1]s.t (email, name)",
			desired:    replaceDesired("UNIQUE (email, name, qty)", "CHECK (qty > 0)"),
			constraint: "t_email_key",
		},
		"a primary key": {
			references: "(id) REFERENCES %[1]s.t (id)",
			desired: `CREATE TABLE t (
		id int,
		email text,
		name text,
		qty int NOT NULL,
		CONSTRAINT t_pkey PRIMARY KEY (id) INCLUDE (qty),
		CONSTRAINT t_email_key UNIQUE (email, name),
		CONSTRAINT t_qty_check CHECK (qty > 0)
	);`,
			constraint: "t_pkey",
		},
	} {
		t.Run(name, func(t *testing.T) {
			pool, schema := replaceConstraintTable(t)
			before, _ := constraintDef(t, pool, schema, tc.constraint)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.r (id int, email text, name text,
				CONSTRAINT r_t_fkey FOREIGN KEY `+tc.references+`)`, schema))
			require.NoError(t, err)

			res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
				Schema: schema, Desired: parseDesired(t, tc.desired),
			}, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
			assert.Contains(t, res.Detail, `constraint "`+tc.constraint+`" is referenced by foreign key "r_t_fkey" on table "`+schema+`"."r"`)
			assert.Contains(t, res.Detail, `drop or change "r_t_fkey" first, or keep the constraint's definition`)

			after, _ := constraintDef(t, pool, schema, tc.constraint)
			assert.Equal(t, before, after)
			assert.NotContains(t, liveIndexes(t, pool, schema), tc.constraint+"_new", "nothing is built")
		})
	}
}
