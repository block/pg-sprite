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
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// columnType is the column's live type as format_type renders it.
func columnType(t *testing.T, pool *pgxpool.Pool, schema, table, column string) string {
	t.Helper()
	var typ string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT pg_catalog.format_type(a.atttypid, a.atttypmod)
		   FROM pg_catalog.pg_attribute a
		  WHERE a.attrelid = pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.' || pg_catalog.quote_ident($2))
		    AND a.attname = $3 AND NOT a.attisdropped`,
		schema, table, column).Scan(&typ))
	return typ
}

// A column a view or materialized view reads cannot be retyped or dropped
// while the view exists; PostgreSQL refuses even a type change it would
// otherwise relabel without a rewrite. The run is refused at plan time with
// the views named, so the operator learns what to change before anything
// reaches the server, and the column is left as it was. A rename, which the
// views follow, and changes to a column no view reads still run.
func TestRunRefusesChangingAColumnAViewReads(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email varchar(50), note text);
			CREATE VIEW %[1]s.active_users AS SELECT id, email FROM %[1]s.users;
			CREATE MATERIALIZED VIEW %[1]s."Mail Report" AS SELECT email FROM %[1]s.users`, schema))
		require.NoError(t, err)
		return schema
	}

	for _, tc := range []struct {
		name      string
		statement string
	}{
		{name: "a type change the server would relabel", statement: "ALTER TABLE %s.users ALTER COLUMN email TYPE varchar(100)"},
		{name: "a type change to text", statement: "ALTER TABLE %s.users ALTER COLUMN email TYPE text"},
		{name: "a column drop", statement: "ALTER TABLE %s.users DROP COLUMN email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
			assert.Contains(t, v.Detail, fmt.Sprintf(`a column read by "%s"."Mail Report", "%s"."active_users"`, schema, schema))
			assert.Contains(t, v.Detail, "drop or redefine those views first, then re-plan")
			assert.Equal(t, "character varying(50)", columnType(t, pool, schema, "users", "email"))
		})
	}

	t.Run("the plan names the views", func(t *testing.T) {
		schema := seed(t)
		st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.users ALTER COLUMN email TYPE varchar(100)", schema))
		facts, err := migrate.LiveFacts(t.Context(), pool, st)
		require.NoError(t, err)
		plan, err := planner.Classify(st.SQL(), facts.Classifier)
		require.NoError(t, err)
		require.Len(t, plan.Decisions, 1)
		assert.Equal(t, planner.ReasonDependentView, plan.Decisions[0].Reason)
		assert.Equal(t, []string{
			fmt.Sprintf(`"%s"."Mail Report"`, schema),
			fmt.Sprintf(`"%s"."active_users"`, schema),
		}, plan.Decisions[0].DependentViews)
	})

	for _, tc := range []struct {
		name      string
		statement string
		check     func(t *testing.T, schema string)
	}{
		{
			name:      "renaming the column the views read",
			statement: "ALTER TABLE %s.users RENAME COLUMN email TO mail",
			check: func(t *testing.T, schema string) {
				assert.True(t, columnExists(t, pool, schema, "users", "mail"))
			},
		},
		{
			name:      "dropping a column no view reads",
			statement: "ALTER TABLE %s.users DROP COLUMN note",
			check: func(t *testing.T, schema string) {
				assert.False(t, columnExists(t, pool, schema, "users", "note"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "detail: %s", v.Detail)
			tc.check(t, schema)
		})
	}
}

// A recursive change reaches the column on every table below, so a view on
// an inheritance child refuses the parent's change with the child's view
// named, while the same drop under ONLY, which leaves the child's column in
// place, runs.
func TestRunRefusesChangingAColumnAChildsViewReads(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email varchar(50));
			CREATE TABLE %[1]s.admins (level int) INHERITS (%[1]s.users);
			CREATE VIEW %[1]s.admin_mail AS SELECT email, level FROM %[1]s.admins`, schema))
		require.NoError(t, err)
		return schema
	}

	for _, statement := range []string{
		"ALTER TABLE %s.users ALTER COLUMN email TYPE varchar(100)",
		"ALTER TABLE %s.users DROP COLUMN email",
	} {
		t.Run(statement, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Contains(t, v.Detail, fmt.Sprintf(`a column read by "%s"."admin_mail"`, schema))
			assert.Equal(t, "character varying(50)", columnType(t, pool, schema, "admins", "email"))
		})
	}

	t.Run("dropping under ONLY", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf("ALTER TABLE ONLY %s.users DROP COLUMN email", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "detail: %s", v.Detail)
		assert.False(t, columnExists(t, pool, schema, "users", "email"))
		assert.True(t, columnExists(t, pool, schema, "admins", "email"))
	})
}
