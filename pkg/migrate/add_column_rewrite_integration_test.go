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

// Adding a column runs in place only when the server proves PostgreSQL
// stores it in the catalog. A constant default on a domain with a CHECK
// would rewrite every row under ACCESS EXCLUSIVE, so it is never executed
// in place; a NOT NULL DEFAULT now() is stored like a constant and is. An
// ADD COLUMN IF NOT EXISTS for a column the table already has does nothing,
// whatever it declares, and completes as the no-op it is.
func TestRunAddColumnFollowsTheProvenRewrite(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	newTable := func(t *testing.T) string {
		schema := testutil.NewSchema(t, pool)
		for _, ddl := range []string{
			"CREATE DOMAIN %[1]s.positive_int AS int CHECK (VALUE > 0)",
			"CREATE TABLE %[1]s.t (id int PRIMARY KEY)",
			"INSERT INTO %[1]s.t SELECT generate_series(1, 100)",
		} {
			_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
			require.NoError(t, err, ddl)
		}
		return schema
	}

	t.Run("a constrained domain is never added in place", func(t *testing.T) {
		schema := newTable(t)
		before := filenode(t, pool, schema+".t")

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %[1]s.t ADD COLUMN score %[1]s.positive_int DEFAULT 1", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
		assert.Equal(t, before, filenode(t, pool, schema+".t"))
		assert.False(t, hasColumn(t, pool, schema, "score"), "the refused change must not touch the schema")
	})

	t.Run("an IF NOT EXISTS for a column the table has is a no-op", func(t *testing.T) {
		schema := newTable(t)
		before := filenode(t, pool, schema+".t")

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %[1]s.t ADD COLUMN IF NOT EXISTS id %[1]s.positive_int DEFAULT 1", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assert.Equal(t, before, filenode(t, pool, schema+".t"))
		var idType string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = $1::regclass AND attname = 'id'",
			schema+".t").Scan(&idType))
		assert.Equal(t, "integer", idType, "the existing column keeps its type")
	})

	t.Run("a stored now() default is added in place", func(t *testing.T) {
		schema := newTable(t)
		before := filenode(t, pool, schema+".t")

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN created timestamptz NOT NULL DEFAULT now()", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assert.Equal(t, before, filenode(t, pool, schema+".t"), "adding the column must not rewrite the table")
		assert.True(t, hasColumn(t, pool, schema, "created"))
	})
}

func filenode(t *testing.T, pool *pgxpool.Pool, table string) uint32 {
	t.Helper()
	var node uint32
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_relation_filenode($1::regclass)", table).Scan(&node))
	return node
}

func hasColumn(t *testing.T, pool *pgxpool.Pool, schema, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't' AND column_name = $2)`, schema, column).Scan(&exists))
	return exists
}
