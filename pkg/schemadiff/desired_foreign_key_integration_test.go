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
	"github.com/block/pg-sprite/pkg/statement"
)

// The scratch build runs with the scratch schema, then the target schema,
// then public on search_path, so whatever the target schema holds is in reach
// of the stand-in index read and the foreign-key binding proof. Both reads
// qualify every relation and operator, and neither comparison admits a
// planted overload in any case: each join compares oid with oid, and a
// comparison with an untyped literal or parameter takes the unknown side as
// the known side's type and finds the catalog's exact operator. The subtests
// therefore hold the two reads to their answers under the build's own path:
// the proof refuses a foreign key that escaped the scratch schema, and a build
// whose foreign key binds to a stand-in completes with the stand-in's copied
// index name free for the file to use.
func TestDesiredForeignKeyScratchReads(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	newFixture := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.customers (id bigint PRIMARY KEY)`, schema))
		require.NoError(t, err, "create the referenced table")
		return schema
	}

	// A foreign key bound to the live referenced table is exactly what the
	// proof exists to refuse; the parse boundary never admits one, so the
	// proof is driven directly with a scratch transaction shaped as the
	// build's: a fresh scratch schema leading the build's search_path.
	t.Run("the binding proof refuses a foreign key bound outside the scratch schema", func(t *testing.T) {
		schema := newFixture(t)
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
		scratch, err := scratchSchemaName()
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{scratch}.Sanitize())
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(scratchSearchPath(scratch, schema)...))
		require.NoError(t, err)

		_, err = tx.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				customer_id bigint CONSTRAINT orders_customer_fk REFERENCES %[2]s.customers (id),
				CONSTRAINT orders_id_check CHECK (id > 0)
			)`, pgx.Identifier{scratch}.Sanitize(), pgx.Identifier{schema}.Sanitize()))
		require.NoError(t, err, "bind a scratch foreign key to the live referenced table")

		err = confirmForeignKeysBindScratch(t.Context(), tx, scratch, "orders")
		require.ErrorIs(t, err, dbconn.ErrInvariantViolation)
		assert.ErrorContains(t, err, `["orders_customer_fk"]`, "the escaped foreign key is named, and the primary key and check constraint are not")
	})

	// LIKE names the stand-in's copied primary key index customers_pkey, the
	// same name a desired file is free to give an index of its own. The
	// stand-in's copy is renamed before the file's statements run, so the
	// file's index lands under that name, and the foreign key, bound to the
	// stand-in, prints as live introspection would print it.
	t.Run("a build whose foreign key binds to a stand-in completes", func(t *testing.T) {
		schema := newFixture(t)
		desiredFile, err := statement.ParseDesired(`
			CREATE TABLE orders (
				id bigint PRIMARY KEY,
				customer_id bigint CONSTRAINT orders_customer_fk REFERENCES customers (id)
			);
			CREATE INDEX customers_pkey ON orders (customer_id)`)
		require.NoError(t, err)
		desired, err := IntrospectDesired(t.Context(), pool, schema, desiredFile)
		require.NoError(t, err)

		var foreignKeys, indexes []string
		for _, c := range desired.Constraints {
			if c.ForeignKey {
				foreignKeys = append(foreignKeys, c.Name+": "+c.Def)
			}
		}
		for _, i := range desired.Indexes {
			indexes = append(indexes, i.Name)
		}
		assert.Equal(t, []string{"orders_customer_fk: FOREIGN KEY (customer_id) REFERENCES customers(id)"}, foreignKeys)
		assert.Contains(t, indexes, "customers_pkey", "the file's index takes the name the stand-in's copied primary key index was renamed away from")
	})
}
