package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// extensionDesiredSQL names extension objects the way an app whose role
// searches an extensions schema writes them: unqualified. citext supplies
// a type and the operator the check uses, uuid-ossp a default's function,
// and pg_trgm an index's operator class.
const extensionDesiredSQL = `
CREATE TABLE accounts (
  id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
  email citext NOT NULL,
  handle text NOT NULL,
  CONSTRAINT email_present CHECK (email <> '')
);
CREATE INDEX accounts_handle_trgm ON accounts USING gin (handle gin_trgm_ops);
`

// extensionDatabase returns a pool on a database of its own with citext,
// uuid-ossp, and pg_trgm installed in a schema named extensions, outside
// every path pg-sprite introspects under. Extensions are database-wide, so
// a throwaway schema cannot isolate them.
func extensionDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, testutil.StartPostgres(t))})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `
		CREATE SCHEMA extensions;
		CREATE EXTENSION citext WITH SCHEMA extensions;
		CREATE EXTENSION "uuid-ossp" WITH SCHEMA extensions;
		CREATE EXTENSION pg_trgm WITH SCHEMA extensions`)
	require.NoError(t, err)
	return pool
}

// A desired file that names extension objects unqualified builds only when
// the caller names the schema they live in. Built that way, it introspects
// to the same model as the live table an app created from the same SQL
// under a path that searches extensions, and the model names every
// extension object qualified, so a plan rendered from it means the same
// objects wherever it runs.
func TestIntrospectDesiredResolvesExtensionSchemas(t *testing.T) {
	pool := extensionDatabase(t)
	schema := testutil.NewSchema(t, pool)

	ds, err := statement.ParseDesired(extensionDesiredSQL)
	require.NoError(t, err)

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(schema, "public", "extensions"))
	require.NoError(t, err)
	for _, st := range ds.Statements() {
		_, err = tx.Exec(t.Context(), st.SQL())
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(t.Context()))

	_, err = schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.ErrorContains(t, err, `type "citext" does not exist`,
		"without extension schemas the file's names resolve exactly as before")

	widened, err := ds.WithExtensionSchemas("extensions")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, widened)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	assert.Equal(t, live, modeltest.Comparable(desired))

	columns := make(map[string]schemadiff.Column)
	for _, col := range desired.Columns {
		columns[col.Name] = col
	}
	assert.Equal(t, "extensions.citext", columns["email"].Type)
	assert.Equal(t, "extensions.uuid_generate_v4()", columns["id"].Default)
	checks := make(map[string]string)
	for _, con := range desired.Constraints {
		checks[con.Name] = con.Def
	}
	assert.Equal(t, "CHECK ((email OPERATOR(extensions.<>) ''::extensions.citext))", checks["email_present"])
	indexes := make(map[string]string)
	for _, idx := range desired.Indexes {
		indexes[idx.Name] = idx.Def
	}
	assert.Equal(t, "CREATE INDEX accounts_handle_trgm ON accounts USING gin (handle extensions.gin_trgm_ops)", indexes["accounts_handle_trgm"])

	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// The extension schemas widen what the file resolves to, never what it
// shadows: a type the target schema defines under the same name as an
// extension's still wins, as it does for the live table.
func TestIntrospectDesiredExtensionSchemasDoNotShadowTheTarget(t *testing.T) {
	pool := extensionDatabase(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE DOMAIN %s.citext AS text", schema))
	require.NoError(t, err)

	ds, err := statement.ParseDesired("CREATE TABLE accounts (id bigint PRIMARY KEY, email citext)")
	require.NoError(t, err)
	widened, err := ds.WithExtensionSchemas("extensions")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, widened)
	require.NoError(t, err)
	for _, col := range desired.Columns {
		if col.Name == "email" {
			assert.Equal(t, "citext", col.Type, "the target schema's domain prints unqualified, as it does for the live table")
		}
	}
}

// The constraints a LIKE clause copies are counted from the table the
// clause copied, resolved on the path the file ran under, so a source the
// file finds only through an extension schema still counts.
func TestIntrospectDesiredCountsConstraintsCopiedFromAnExtensionSchema(t *testing.T) {
	pool := extensionDatabase(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), "CREATE TABLE extensions.template (b int UNIQUE)")
	require.NoError(t, err)

	ds, err := statement.ParseDesired(`
		CREATE TABLE t (
		  LIKE template INCLUDING INDEXES,
		  a int UNIQUE
		);`)
	require.NoError(t, err)
	widened, err := ds.WithExtensionSchemas("extensions")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, widened)
	require.NoError(t, err)
	defs := make(map[string]string)
	for _, con := range desired.Constraints {
		defs[con.Name] = con.Def
	}
	assert.Equal(t, map[string]string{"t_a_key": "UNIQUE (a)", "t_b_key": "UNIQUE (b)"}, defs)
}
