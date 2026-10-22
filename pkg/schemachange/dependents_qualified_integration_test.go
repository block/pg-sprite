package schemachange

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The dependent reads run on whatever path their caller set, including
// the shadow build's source-schema-then-public path, so every function
// and operator they call must be the catalog's (CO-9). Function and
// operator resolution ranks every schema on the path, so an exact-type
// overload in the source schema beats the catalog's cast-requiring one
// even with pg_catalog searched first. Here one overload per function
// family the two reads call is planted in the schema, and readIndexes
// and readStatistics must give the same answer on a path that lists
// the schema as on the catalog alone.
func TestDependentReadsIgnoreEveryPlantedOverload(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	q := pgx.Identifier{schema}.Sanitize()

	// Every index carries something a hijacked call would change: key
	// options and a predicate, an included column, a non-default opclass,
	// and a collation other than the column's own, which only the indkey
	// unnest feeds.
	_, err = pool.Exec(t.Context(), `
		CREATE TABLE `+q+`.orders (
			id bigint PRIMARY KEY,
			a integer,
			c integer,
			b text COLLATE "C",
			d integer
		);
		CREATE INDEX orders_inc ON `+q+`.orders (a DESC NULLS LAST, c) INCLUDE (d) WHERE d > 0;
		CREATE UNIQUE INDEX orders_b ON `+q+`.orders (b text_pattern_ops);
		CREATE INDEX orders_bd ON `+q+`.orders (b COLLATE "default");
		CREATE STATISTICS `+q+`.orders_st (ndistinct, dependencies) ON a, c FROM `+q+`.orders;`)
	require.NoError(t, err)

	read := func(path ...string) ([]indexEntry, []dependent) {
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { assert.NoError(t, tx.Rollback(t.Context())) }()
		_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(path...))
		require.NoError(t, err)
		oid := relationOID(t, tx, schema, "orders")
		indexes, err := readIndexes(t.Context(), tx, oid)
		require.NoError(t, err)
		statistics, err := readStatistics(t.Context(), tx, oid)
		require.NoError(t, err)
		return indexes, statistics
	}
	wantIndexes, wantStatistics := read("pg_catalog")
	require.Len(t, wantIndexes, 4)
	require.Len(t, wantStatistics, 1)

	// Each plant matches the argument types the read passes exactly and
	// answers wrongly: generate_series drops the last position, the
	// unnests return one constant row, array_to_string and || return a
	// constant, and to_jsonb claims NULLS NOT DISTINCT.
	_, err = pool.Exec(t.Context(), `
		CREATE FUNCTION `+q+`.generate_series(integer, smallint) RETURNS SETOF integer LANGUAGE sql
			AS 'SELECT pg_catalog.generate_series($1, $2::pg_catalog.int4 OPERATOR(pg_catalog.-) 1)';
		CREATE FUNCTION `+q+`.unnest(oid[]) RETURNS SETOF oid LANGUAGE sql
			AS 'SELECT 0::pg_catalog.oid';
		CREATE FUNCTION `+q+`.unnest(smallint[]) RETURNS SETOF smallint LANGUAGE sql
			AS 'SELECT 0::pg_catalog.int2';
		CREATE FUNCTION `+q+`.unnest("char"[]) RETURNS SETOF "char" LANGUAGE sql
			AS 'SELECT ''z''::pg_catalog."char"';
		CREATE FUNCTION `+q+`.array_to_string("char"[], text) RETURNS text LANGUAGE sql
			AS 'SELECT ''planted''::pg_catalog.text';
		CREATE FUNCTION `+q+`.to_jsonb(pg_catalog.pg_index) RETURNS jsonb LANGUAGE sql
			AS 'SELECT ''{"indnullsnotdistinct": "true"}''::pg_catalog.jsonb';
		CREATE FUNCTION `+q+`.cat(text, name) RETURNS text LANGUAGE sql
			AS 'SELECT ''planted''::pg_catalog.text';
		CREATE OPERATOR `+q+`.|| (LEFTARG = text, RIGHTARG = name, FUNCTION = `+q+`.cat);`)
	require.NoError(t, err)

	gotIndexes, gotStatistics := read(schema, "public")
	assert.Equal(t, wantIndexes, gotIndexes)
	assert.Equal(t, wantStatistics, gotStatistics)
}
