package preflight_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

func TestCheckTableAbsentProvesFreeName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, "brand_new")
	require.NoError(t, err)
	assert.Equal(t, schema, at.Schema())
	assert.Equal(t, "brand_new", at.Table())
}

// An unqualified check resolves the schema an unqualified CREATE TABLE
// would land in, so the proof names the exact creation target.
func TestCheckTableAbsentResolvesUnqualifiedName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	var creationSchema string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&creationSchema))

	at, err := preflight.CheckTableAbsent(t.Context(), pool, "", "brand_new")
	require.NoError(t, err)
	assert.Equal(t, creationSchema, at.Schema())
	assert.Equal(t, "brand_new", at.Table())
}

// Any relation kind occupies the name: a CREATE TABLE collides with a view
// or sequence exactly as it does with a table.
func TestCheckTableAbsentRefusesOccupiedName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.taken_table (id int)", schema),
		fmt.Sprintf("CREATE VIEW %s.taken_view AS SELECT 1", schema),
		fmt.Sprintf("CREATE SEQUENCE %s.taken_seq", schema),
	} {
		_, err = pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	for _, name := range []string{"taken_table", "taken_view", "taken_seq"} {
		_, err := preflight.CheckTableAbsent(t.Context(), pool, schema, name)
		assert.ErrorIs(t, err, preflight.ErrRelationExists, "occupied name %s", name)
	}
}

func TestCheckTableAbsentMissingSchema(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	_, err = preflight.CheckTableAbsent(t.Context(), pool, "no_such_schema", "t")
	assert.ErrorIs(t, err, preflight.ErrSchemaNotFound)
}

// The catalog is matched on the exact name, so a mixed-case relation blocks
// only its exact spelling — the lowercase name is genuinely free.
func TestCheckTableAbsentMatchesExactName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s."Weird Name" (id int)`, schema))
	require.NoError(t, err)

	_, err = preflight.CheckTableAbsent(t.Context(), pool, schema, "Weird Name")
	assert.ErrorIs(t, err, preflight.ErrRelationExists)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, "weird name")
	require.NoError(t, err)
	assert.Equal(t, "weird name", at.Table())
}

// A session whose search_path names no schema has no creation target for an
// unqualified name; the check fails rather than guessing a schema.
func TestCheckTableAbsentRefusesEmptySearchPath(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	defer admin.Close()

	const password = "absent-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER ROLE %s SET search_path = ''",
		pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)

	pool := connectAs(t, serverURL, role, password)
	_, err = preflight.CheckTableAbsent(t.Context(), pool, "", "t")
	require.Error(t, err)
	assert.NotErrorIs(t, err, preflight.ErrSchemaNotFound,
		"an unresolvable search_path is not a missing schema — there is no schema name to report missing")
}
