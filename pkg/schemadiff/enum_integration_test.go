package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The enum read gathers its labels with array_agg over text. A caller's
// pool whose search_path holds a planted array_agg(text) resolves an
// unqualified call to the planted aggregate, because the catalog's takes
// text through a polymorphic parameter. The planted one answers with the
// labels a desired file lists, so a read that left array_agg unqualified
// would report a type short of 'paid' as already carrying it, and the plan
// would call the type converged. The read must report the labels the
// catalog holds, in sort order, and the plan must still add 'paid'.
func TestIntrospectEnumTypeResistsAPlantedArrayAgg(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)
	// The real type is created before the impostor, so its DDL never runs
	// with the impostor in reach.
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.order_status AS ENUM ('new', 'shipped')`, schema))
	require.NoError(t, err, "create the real type")
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %[1]s.impostor_step(text[], text) RETURNS text[]
			LANGUAGE sql IMMUTABLE AS 'SELECT ARRAY[''new'', ''paid'', ''shipped'']::text[]';
		CREATE AGGREGATE %[1]s.array_agg(text) (SFUNC = %[1]s.impostor_step, STYPE = text[])`, schema))
	require.NoError(t, err, "plant the impostor")

	// A raw pgx pool on purpose: a library caller's pool carries whatever
	// search_path its owner gave it, not pkg/dbconn's session defaults.
	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err, "parse server URL")
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err, "connect with the planted aggregate on the search_path")
	t.Cleanup(pool.Close)

	var planted string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT array_agg(v)::text FROM (VALUES ('new'::text)) AS o(v)`).Scan(&planted))
	require.Equal(t, "{new,paid,shipped}", planted, "the impostor must answer an unqualified array_agg on this pool")

	live, err := schemadiff.IntrospectEnumType(t.Context(), pool, schema, "order_status")
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "shipped"}, live.Labels, "the catalog's labels, in sort order")

	file, err := statement.ParseDesiredFile(`CREATE TYPE order_status AS ENUM ('new', 'paid', 'shipped');`)
	require.NoError(t, err)
	desired, ok := file.EnumType()
	require.True(t, ok, "the file declares an enum type")
	changes, err := schemadiff.DiffEnumType(live, desired)
	require.NoError(t, err)
	require.Len(t, changes, 1, "the type still lacks 'paid'")
	assert.Equal(t, fmt.Sprintf("ALTER TYPE %s.order_status ADD VALUE 'paid' BEFORE 'shipped'", schema), changes[0].SQL)
}
