package schemadiff

import (
	"context"
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
	t.Cleanup(pool.Close)

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

// An extension's member tables are created and dropped with the extension,
// so no desired file declares them. The stock integration image ships no
// extension that creates a table, so the test makes one a member of the
// extension every database carries — the catalog records the same
// extension dependency an extension script would — and proves the listing
// leaves it out while an unowned neighbour stays in.
func TestListManagedTablesExcludesExtensionOwnedRelations(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	qualified := pgx.Identifier{schema}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.owned_by_ext (id bigint);
		CREATE TABLE %[1]s.unowned (id bigint);
		ALTER EXTENSION plpgsql ADD TABLE %[1]s.owned_by_ext;`, qualified))
	require.NoError(t, err)
	t.Cleanup(func() {
		// The schema drop cannot cascade to an extension member; release the
		// membership first so the throwaway schema's own cleanup succeeds.
		_, err := pool.Exec(context.WithoutCancel(t.Context()),
			fmt.Sprintf("ALTER EXTENSION plpgsql DROP TABLE %s.owned_by_ext", qualified))
		require.NoError(t, err)
	})

	tables, err := ListManagedTables(t.Context(), pool, schema)
	require.NoError(t, err)

	assert.Equal(t, []string{"unowned"}, tables)
}
