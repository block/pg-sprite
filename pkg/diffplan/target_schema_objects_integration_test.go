package diffplan_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
)

// A table in a schema other than public can use a type or function that
// its own schema defines: an enum column, a check constraint calling a
// function, an index over a function. A desired file that declares the
// live table exactly plans nothing, whether it names the object bare or
// qualified with the schema, so the same file is never planned as a drop
// and re-add of the constraint or index, or as a retype of the column.
// SCHEMA in a desired file stands for the target schema's name.
func TestPlanResolvesTheTargetSchemasOwnTypesAndFunctions(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	const enumLive = "CREATE TYPE %[1]s.mood AS ENUM ('ok', 'sad'); CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, m %[1]s.mood)"
	const checkLive = "CREATE FUNCTION %[1]s.positive(n int) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT n > 0';" +
		" CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, a int, CONSTRAINT t_a_check CHECK (%[1]s.positive(a)))"
	const indexLive = "CREATE FUNCTION %[1]s.norm(s text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT lower(s)';" +
		" CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, e text); CREATE INDEX t_norm_idx ON %[1]s.t (%[1]s.norm(e))"
	cases := []struct {
		name    string
		live    string
		desired string
	}{
		{"an enum named bare", enumLive, "CREATE TABLE t (id bigint PRIMARY KEY, m mood);"},
		{"an enum named qualified", enumLive, "CREATE TABLE t (id bigint PRIMARY KEY, m SCHEMA.mood);"},
		{"a check function named bare", checkLive, "CREATE TABLE t (id bigint PRIMARY KEY, a int, CONSTRAINT t_a_check CHECK (positive(a)));"},
		{"a check function named qualified", checkLive, "CREATE TABLE t (id bigint PRIMARY KEY, a int, CONSTRAINT t_a_check CHECK (SCHEMA.positive(a)));"},
		{"an index function named bare", indexLive, "CREATE TABLE t (id bigint PRIMARY KEY, e text);\nCREATE INDEX t_norm_idx ON t (norm(e));"},
		{"an index function named qualified", indexLive, "CREATE TABLE t (id bigint PRIMARY KEY, e text);\nCREATE INDEX t_norm_idx ON t (SCHEMA.norm(e));"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(c.live, schema))
			require.NoError(t, err)

			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
				Schema:  schema,
				Desired: parseDesired(t, strings.ReplaceAll(c.desired, "SCHEMA", schema)),
			})
			require.NoError(t, err)
			assert.Empty(t, report.Statements, "a file that declares the live table plans nothing")
		})
	}
}

// A table in a schema other than public can still use a type installed in
// public, where extensions put theirs, by its bare name. The type lives in
// a database of its own, so it is gone with the database.
func TestPlanResolvesPublicTypesFromAnotherSchema(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(t.Context(), "CREATE TYPE public.shared_mood AS ENUM ('ok', 'sad')")
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigint PRIMARY KEY, m public.shared_mood)", schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE t (id bigint PRIMARY KEY, m shared_mood);"),
	})
	require.NoError(t, err)
	assert.Empty(t, report.Statements, "a file that declares the live table plans nothing")
}

// Index edge cases a desired file can state that the server rewrites or
// resolves on its own: a name longer than the server keeps, an expression
// over a built-in the target schema shadows, and a dropped column whose
// index goes with it. Each plans what the file means and nothing more.
func TestPlanIndexEdgeCases(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	t.Run("an index name the server truncates", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		name := "t_" + strings.Repeat("x", 70)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, a int); CREATE INDEX %[2]s ON %[1]s.t (a)", schema, name))
		require.NoError(t, err)

		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
			Schema:  schema,
			Desired: parseDesired(t, fmt.Sprintf("CREATE TABLE t (id bigint PRIMARY KEY, a int);\nCREATE INDEX %s ON t (a);", name)),
		})
		require.NoError(t, err)
		assert.Empty(t, report.Statements, "both sides keep the same 63-byte prefix of the name")
	})

	t.Run("a built-in the target schema shadows", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE FUNCTION %[1]s.lower(s text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT upper(s)';
			CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, e text);
			CREATE INDEX t_lower_idx ON %[1]s.t (pg_catalog.lower(e))`, schema))
		require.NoError(t, err)

		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
			Schema:  schema,
			Desired: parseDesired(t, "CREATE TABLE t (id bigint PRIMARY KEY, e text);\nCREATE INDEX t_lower_idx ON t (lower(e));"),
		})
		require.NoError(t, err)
		assert.Empty(t, report.Statements, "a bare lower() is the built-in on both sides, as pg_catalog is searched first")
	})

	t.Run("a dropped column takes its index", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, a int); CREATE INDEX t_a_idx ON %[1]s.t (a)", schema))
		require.NoError(t, err)

		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
			Schema:  schema,
			Desired: parseDesired(t, "CREATE TABLE t (id bigint PRIMARY KEY);"),
		})
		require.NoError(t, err)
		require.Len(t, report.Statements, 2)
		assert.Equal(t, fmt.Sprintf("DROP INDEX %s.t_a_idx", schema), report.Statements[0].SQL, "the index drop runs first, as its own bounded step")
		assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.t DROP a", schema), report.Statements[1].SQL)
	})
}
