package statement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// Two-oracle check (TM): for each representative CREATE TABLE, the names
// ImplicitIndexNames predicts must be exactly the index names the real
// server mints when it runs the same statement into an empty schema. The
// prediction is the server's first choice, and an empty schema guarantees
// the first choice is what the catalog records.
func TestImplicitIndexNamesMatchServer(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	longTable := strings.Repeat("a", 60)
	tests := []struct {
		name string
		sql  string
	}{
		{name: "unnamed inline primary key", sql: "CREATE TABLE t (id int PRIMARY KEY)"},
		{name: "unnamed table-constraint primary key", sql: "CREATE TABLE t (id int, PRIMARY KEY (id))"},
		{name: "multi-column unique table constraint", sql: "CREATE TABLE t (a int, b int, UNIQUE (a, b))"},
		{name: "inline unique column", sql: "CREATE TABLE t (id int, email text UNIQUE)"},
		{name: "named unique constraint", sql: "CREATE TABLE t (id int, CONSTRAINT my_uni UNIQUE (id))"},
		{name: "primary key and unique together", sql: "CREATE TABLE t (id int PRIMARY KEY, a int, b int, UNIQUE (a, b))"},
		{name: "long table name truncates the generated name", sql: fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY)", longTable)},
		{name: "btree exclusion constraint", sql: "CREATE TABLE t (c int, EXCLUDE USING btree (c WITH =))"},
		{name: "exclusion constraint over an expression", sql: "CREATE TABLE t (c int, EXCLUDE USING btree ((c + 1) WITH =))"},
		{name: "no index-backed constraints", sql: "CREATE TABLE t (id int, note text)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicted, err := statement.ImplicitIndexNames(tt.sql)
			require.NoError(t, err)

			schema := testutil.NewSchema(t, pool)
			tx, err := pool.Begin(t.Context())
			require.NoError(t, err)
			defer func() { assert.NoError(t, tx.Commit(t.Context())) }()
			_, err = tx.Exec(t.Context(), "SET LOCAL search_path = "+schema)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), tt.sql)
			require.NoError(t, err)

			rows, err := tx.Query(t.Context(),
				`SELECT ic.relname
				   FROM pg_index i
				   JOIN pg_class c ON c.oid = i.indrelid
				   JOIN pg_class ic ON ic.oid = i.indexrelid
				   JOIN pg_namespace n ON n.oid = c.relnamespace
				  WHERE n.nspname = $1
				  ORDER BY ic.oid`, schema)
			require.NoError(t, err)
			var actual []string
			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))
				actual = append(actual, name)
			}
			require.NoError(t, rows.Err())

			assert.ElementsMatch(t, predicted, actual,
				"predicted first-choice names must match the names the server minted")
		})
	}
}
