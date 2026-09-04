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
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, "brand_new")
	require.NoError(t, err)
	assert.Equal(t, schema, at.Schema())
	assert.Equal(t, "brand_new", at.Table())
}

func TestCheckNamesAbsent(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	otherSchema := testutil.NewSchema(t, pool)
	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, "new_table")
	require.NoError(t, err)

	require.NoError(t, preflight.CheckNamesAbsent(t.Context(), pool, at, nil))
	require.NoError(t, preflight.CheckNamesAbsent(t.Context(), pool, at, []string{"free_name"}))
	require.Error(t, preflight.CheckNamesAbsent(t.Context(), pool, preflight.AbsentTarget{}, []string{"free_name"}))
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (v int)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX t_pkey ON %s.other (v)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t_pkey (v int)", otherSchema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other_schema_only (v int)", otherSchema))
	require.NoError(t, err)

	err = preflight.CheckNamesAbsent(t.Context(), pool, at, []string{"z_free", "t_pkey"})
	assert.ErrorIs(t, err, preflight.ErrRelationExists)
	assert.Contains(t, err.Error(), "t_pkey")
	require.NoError(t, preflight.CheckNamesAbsent(t.Context(), pool, at, []string{"other_schema_only"}))
}

// An unqualified check resolves the schema an unqualified CREATE TABLE
// would land in, so the proof names the exact creation target.
func TestCheckTableAbsentResolvesUnqualifiedName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var creationSchema string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&creationSchema))

	at, err := preflight.CheckTableAbsent(t.Context(), pool, "", "brand_new")
	require.NoError(t, err)
	assert.Equal(t, creationSchema, at.Schema())
	assert.Equal(t, "brand_new", at.Table())
}

// Any relation kind occupies the name: a CREATE TABLE collides with a view
// or sequence exactly as it does with a table. taken_idx is a double
// occupant — an index has no pg_type row, so a standalone type shares its
// name — pinning that the relation wins the report: the relation is what
// blocks a CREATE TABLE, and ErrTypeExists would name the wrong obstacle.
func TestCheckTableAbsentRefusesOccupiedName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.taken_table (id int)", schema),
		fmt.Sprintf("CREATE VIEW %s.taken_view AS SELECT 1", schema),
		fmt.Sprintf("CREATE SEQUENCE %s.taken_seq", schema),
		fmt.Sprintf("CREATE INDEX taken_idx ON %s.taken_table (id)", schema),
		fmt.Sprintf("CREATE TYPE %s.taken_idx AS ENUM ('a')", schema),
		fmt.Sprintf("CREATE MATERIALIZED VIEW %s.taken_mv AS SELECT 1", schema),
		fmt.Sprintf("CREATE TABLE %s.taken_part (id int) PARTITION BY RANGE (id)", schema),
		fmt.Sprintf("CREATE TYPE %s.taken_comp AS (x int)", schema),
	} {
		_, err = pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	for _, name := range []string{
		"taken_table", "taken_view", "taken_seq", "taken_idx", "taken_mv", "taken_part", "taken_comp",
	} {
		_, err := preflight.CheckTableAbsent(t.Context(), pool, schema, name)
		assert.ErrorIs(t, err, preflight.ErrRelationExists, "occupied name %s", name)
		assert.True(t, preflight.IsNameOccupied(err), "occupied name %s", name)
	}
}

// Standalone types occupy the name too: every table gets a composite type
// of its own name, so an enum, domain, or range at the target collides with
// a CREATE TABLE even though pg_class knows nothing about it.
func TestCheckTableAbsentRefusesOccupiedTypeName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TYPE %s.taken_enum AS ENUM ('a')", schema),
		fmt.Sprintf("CREATE DOMAIN %s.taken_domain AS int", schema),
		fmt.Sprintf("CREATE TYPE %s.taken_rg AS RANGE (subtype = int4)", schema),
	} {
		_, err = pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	for _, name := range []string{"taken_enum", "taken_domain", "taken_rg"} {
		_, err := preflight.CheckTableAbsent(t.Context(), pool, schema, name)
		assert.ErrorIs(t, err, preflight.ErrTypeExists, "occupied type name %s", name)
		assert.True(t, preflight.IsNameOccupied(err), "occupied type name %s", name)

		// The refusal matches the server's behavior: the create really
		// does collide with the type, so refusing was not a false block.
		_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.%s (id int)", schema, name))
		assert.Error(t, err, "CREATE TABLE %s should collide with the type", name)
	}
}

// An autogenerated array type does not occupy its name: CREATE TABLE renames
// it out of the way, so the check must not refuse a name that a create
// would in fact win.
func TestCheckTableAbsentIgnoresAutogeneratedArrayType(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	// CREATE TYPE autogenerates the array type _taken_rg alongside taken_rg.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TYPE %s.taken_rg AS RANGE (subtype = int4)", schema))
	require.NoError(t, err)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, "_taken_rg")
	require.NoError(t, err)
	assert.Equal(t, "_taken_rg", at.Table())

	// The proof matches the server's behavior: the create really succeeds.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s."_taken_rg" (id int)`, schema))
	assert.NoError(t, err)
}

// An empty table name names nothing; the check fails closed rather than
// minting a proof for a target no CREATE TABLE could have.
func TestCheckTableAbsentRefusesEmptyTableName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = preflight.CheckTableAbsent(t.Context(), pool, schema, "")
	require.Error(t, err)
}

// Occupancy is a catalog fact, not a privilege question: a role with no
// grants on the schema still sees the occupant, so a missing grant can
// never masquerade as absence.
func TestCheckTableAbsentSeesOccupantWithoutPrivileges(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)

	_, err = admin.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.hidden_table (id int)", schema))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("REVOKE ALL ON SCHEMA %s FROM PUBLIC", schema))
	require.NoError(t, err)

	const password = "absent-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	pool := connectAs(t, serverURL, role, password)

	_, err = preflight.CheckTableAbsent(t.Context(), pool, schema, "hidden_table")
	assert.ErrorIs(t, err, preflight.ErrRelationExists)
}

func TestCheckTableAbsentMissingSchema(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = preflight.CheckTableAbsent(t.Context(), pool, "no_such_schema", "t")
	assert.ErrorIs(t, err, preflight.ErrSchemaNotFound)
	assert.False(t, preflight.IsNameOccupied(err),
		"a missing schema is not an occupied name — the causes route differently")
}

// The catalog is matched on the exact name, so a mixed-case relation blocks
// only its exact spelling — the lowercase name is genuinely free.
func TestCheckTableAbsentMatchesExactName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
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
	t.Cleanup(admin.Close)

	const password = "absent-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER ROLE %s SET search_path = ''",
		pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)

	pool := connectAs(t, serverURL, role, password)
	_, err = preflight.CheckTableAbsent(t.Context(), pool, "", "t")
	assert.ErrorIs(t, err, preflight.ErrNoCreationSchema)
	assert.NotErrorIs(t, err, preflight.ErrSchemaNotFound,
		"an unresolvable search_path is not a missing schema — there is no schema name to report missing")
}

// CheckTable and CheckTableAbsent are not inverses for an unqualified name:
// CheckTable resolves across the whole search_path while CheckTableAbsent
// resolves current_schema() only. A table in a later search_path schema
// makes both checks succeed for the same arguments — the documented reason
// a caller deciding between create and alter must qualify the schema.
func TestCheckTableAbsentIsNotComplementOfCheckTable(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	first := testutil.NewSchema(t, admin)
	second := testutil.NewSchema(t, admin)

	_, err = admin.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.only_here (id int)", second))
	require.NoError(t, err)

	const password = "absent-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	_, err = admin.Exec(t.Context(), fmt.Sprintf("GRANT USAGE ON SCHEMA %s, %s TO %s",
		first, second, pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER ROLE %s SET search_path = %s, %s",
		pgx.Identifier{role}.Sanitize(), first, second))
	require.NoError(t, err)
	pool := connectAs(t, serverURL, role, password)

	// CheckTable finds the table through the search_path...
	pt, err := preflight.CheckTable(t.Context(), pool, "", "only_here", preflight.NoSizeLimit)
	require.NoError(t, err)
	assert.Equal(t, "only_here", pt.Table())

	// ...while CheckTableAbsent proves the same name free, because an
	// unqualified CREATE TABLE would land in the first schema, not the
	// second. Both proofs are true at once.
	at, err := preflight.CheckTableAbsent(t.Context(), pool, "", "only_here")
	require.NoError(t, err)
	assert.Equal(t, first, at.Schema())
}
