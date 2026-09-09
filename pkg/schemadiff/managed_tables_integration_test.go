package schemadiff

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestListManagedTables(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	t.Run("lists file-backed table kinds", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		qualified := pgx.Identifier{schema}.Sanitize()
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.ordinary (id bigint);
			CREATE TABLE %[1]s.partitioned (id bigint) PARTITION BY RANGE (id);
			CREATE TABLE %[1]s.partitioned_first PARTITION OF %[1]s.partitioned
				FOR VALUES FROM (0) TO (10);
			CREATE TABLE %[1]s.inheritance_parent (id bigint);
			CREATE TABLE %[1]s.inheritance_child () INHERITS (%[1]s.inheritance_parent);
			CREATE UNLOGGED TABLE %[1]s.unlogged (id bigint);`, qualified))
		require.NoError(t, err)

		tables, err := ListManagedTables(t.Context(), pool, schema)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"inheritance_child",
			"inheritance_parent",
			"ordinary",
			"partitioned",
			"unlogged",
		}, tables)
	})

	t.Run("empty schema", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)

		tables, err := ListManagedTables(t.Context(), pool, schema)
		require.NoError(t, err)
		assert.Empty(t, tables)
		assert.NotNil(t, tables)
	})
}

// The stock PostgreSQL integration image has no extension that owns tables.
// Keep the catalog dependency exclusion explicit and protected from removal.
func TestListManagedTablesExcludesExtensionOwnedRelations(t *testing.T) {
	assert.Contains(t, listManagedTablesSQL, "d.deptype OPERATOR(pg_catalog.=) 'e'")
}
