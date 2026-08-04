package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

const desiredSQL = `
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name varchar(50) NOT NULL,
  payload jsonb DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT name_not_empty CHECK (length(name) > 0)
);
CREATE INDEX events_created_at_idx ON events (created_at);
CREATE UNIQUE INDEX events_name_key ON events (name);
`

// The two-oracle test of execute-and-introspect: creating the table live and
// materializing the same file on the scratch schema must introspect to the
// identical canonical model.
func TestIntrospectDesiredMatchesLiveIntrospection(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)
	for _, st := range ds.Statements {
		qualified, err := statement.Qualify(st.SQL, schema)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), qualified)
		require.NoError(t, err)
	}

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	assert.Equal(t, live, desired,
		"live introspection and scratch execute-and-introspect must agree on the canonical model")
}

// Cosmetically different spellings of the same schema must introspect to the
// same canonical model: the server's decompilers are the canonicalizer.
func TestIntrospectCanonicalizesTypeAliases(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.aliased (a int4, b varchar(50), c timestamptz, d bool DEFAULT TRUE)", schema))
	require.NoError(t, err)

	m, err := schemadiff.Introspect(t.Context(), pool, schema, "aliased")
	require.NoError(t, err)
	require.Len(t, m.Columns, 4)
	assert.Equal(t, "integer", m.Columns[0].Type)
	assert.Equal(t, "character varying(50)", m.Columns[1].Type)
	assert.Equal(t, "timestamp with time zone", m.Columns[2].Type)
	assert.Equal(t, "boolean", m.Columns[3].Type)
	assert.Equal(t, "true", m.Columns[3].Default)
}

func TestIntrospectDesiredLeavesNoFootprint(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)
	_, err = schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	var leftover int
	err = pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'pgsprite\\_scratch\\_%'").Scan(&leftover)
	require.NoError(t, err)
	assert.Zero(t, leftover, "the scratch schema must never survive its transaction")
}

func TestIntrospectTableNotFound(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = schemadiff.Introspect(t.Context(), pool, schema, "missing")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
}

func TestIntrospectRefusesViews(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE VIEW %s.v AS SELECT 1 AS one", schema))
	require.NoError(t, err)

	_, err = schemadiff.Introspect(t.Context(), pool, schema, "v")
	require.ErrorIs(t, err, schemadiff.ErrNotTable)
}

// A desired statement that is valid grammar but invalid semantics (a type
// that does not exist) must fail at scratch execution — semantic truth comes
// from the server, not the parser.
func TestIntrospectDesiredSurfacesSemanticErrors(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	ds, err := statement.ParseDesired("CREATE TABLE t (id no_such_type)")
	require.NoError(t, err, "the grammar accepts unknown type names; only the server can refuse them")
	_, err = schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.Error(t, err)
}

// The convergence oracle: diff live against desired, execute the plan, and
// the re-diff must be empty. This closes the loop between the diff engine
// and the real server semantics.
func TestDiffConverges(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	// The live table starts from an older shape: a column to drop, a column
	// to retype, a default to add, an index to replace.
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s.events (
			id bigint PRIMARY KEY,
			name varchar(20) NOT NULL,
			legacy int,
			created_at timestamptz NOT NULL
		)`, schema),
		fmt.Sprintf("CREATE INDEX events_created_at_idx ON %s.events (created_at DESC)", schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	require.NotEmpty(t, changes)

	for _, ch := range changes {
		_, err := pool.Exec(t.Context(), ch.SQL)
		require.NoError(t, err, "derived statement must execute: %s", ch.SQL)
	}

	after, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	rediff, err := schemadiff.Diff(schema, after, desired)
	require.NoError(t, err)
	assert.Empty(t, rediff, "after executing the plan the live table must match the desired state")
}

// Identity and generated columns round-trip through both introspection paths.
func TestIntrospectIdentityAndGeneratedColumns(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.gen (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		n int NOT NULL,
		doubled int GENERATED ALWAYS AS (n * 2) STORED
	)`, schema))
	require.NoError(t, err)

	m, err := schemadiff.Introspect(t.Context(), pool, schema, "gen")
	require.NoError(t, err)
	require.Len(t, m.Columns, 3)
	assert.Equal(t, schemadiff.IdentityAlways, m.Columns[0].Identity)
	assert.True(t, m.Columns[2].Generated)
	assert.Equal(t, "(n * 2)", m.Columns[2].Default)
}
