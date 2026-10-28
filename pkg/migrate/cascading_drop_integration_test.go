package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A drop with CASCADE also drops whatever depends on its target: here a view
// that reads the column, and another table's foreign key on the primary
// key. Neither is named in the statement, so the run is refused at plan
// time and tells the operator to drop the dependents explicitly. Nothing
// reaches the server: the column, the constraint, the view, and the other
// table's foreign key are all still there. The same drops without CASCADE,
// on a target nothing depends on, still run.
func TestRunRefusesACascadingDrop(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text, note text);
			CREATE VIEW %[1]s.active_users AS SELECT id, email FROM %[1]s.users;
			CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint REFERENCES %[1]s.users (id))`, schema))
		require.NoError(t, err)
		return schema
	}
	dependentsRemain := func(t *testing.T, schema string) {
		t.Helper()
		assert.True(t, columnExists(t, pool, schema, "users", "email"))
		var views, keys int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) 'active_users'`, schema).Scan(&views))
		assert.Equal(t, 1, views, "the view that reads the column")
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_catalog.pg_constraint
			  WHERE conrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.orders')
			    AND contype OPERATOR(pg_catalog.=) 'f'`, schema).Scan(&keys))
		assert.Equal(t, 1, keys, "the other table's foreign key on the primary key")
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_catalog.pg_constraint
			  WHERE conrelid OPERATOR(pg_catalog.=) pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.users')
			    AND contype OPERATOR(pg_catalog.=) 'p'`, schema).Scan(&keys))
		assert.Equal(t, 1, keys, "the primary key")
	}

	for _, tc := range []struct {
		name      string
		statement string
		operation string
	}{
		{
			name:      "a column a view reads",
			statement: "ALTER TABLE %s.users DROP COLUMN email CASCADE",
			operation: "DROP COLUMN email",
		},
		{
			name:      "a primary key another table's foreign key references",
			statement: "ALTER TABLE %s.users DROP CONSTRAINT users_pkey CASCADE",
			operation: "DROP CONSTRAINT users_pkey",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
			assert.Contains(t, v.Detail, tc.operation+" uses CASCADE, which also drops the objects that depend on it")
			assert.Contains(t, v.Detail, "drop each dependent object explicitly, then resubmit without CASCADE")
			dependentsRemain(t, schema)
		})
	}

	t.Run("a drop without CASCADE on a column nothing depends on", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf("ALTER TABLE %s.users DROP COLUMN note", schema)), destructiveOptions(schema+".users"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "detail: %s", v.Detail)
		assert.False(t, columnExists(t, pool, schema, "users", "note"))
		dependentsRemain(t, schema)
	})
}
