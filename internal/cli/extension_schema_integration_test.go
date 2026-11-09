package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// An app keeps citext in an extensions schema and writes `email citext`
// unqualified. With --extension-schema naming that schema, migrate
// --desired creates the missing table, the next run finds nothing to do,
// and a column added to the file later is planned with its type qualified
// and converges. The same file without the flag stops before creating
// anything.
func TestMigrateDesiredResolvesExtensionSchema(t *testing.T) {
	url, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)

	const created = `CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL
 );`
	var out strings.Builder
	require.Error(t, desiredCommand(t, url, schema, created).run(t.Context(), &out))
	_, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound, "an unresolved type creates nothing")

	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, created, "--extension-schema", "extensions").run(t.Context(), &out))
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "extensions.citext", live.Columns[1].Type)

	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, created, "--extension-schema", "extensions").run(t.Context(), &out))
	result = migrate.DesiredResult{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	assert.Empty(t, result.Plan.Statements, "the created table already matches the file")

	const added = `CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL,
 alias citext
 );`
	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, added, "--extension-schema", "extensions").run(t.Context(), &out))
	result = migrate.DesiredResult{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	require.Len(t, result.Plan.Statements, 1)
	assert.Equal(t, "ALTER TABLE "+schema+".accounts ADD COLUMN alias extensions.citext", result.Plan.Statements[0].SQL)
	require.NotNil(t, result.Convergence)
	live, err = schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 3)
	assert.Equal(t, "extensions.citext", live.Columns[2].Type)
}

// extensionSchemaDatabase returns the URL of a database of its own with
// citext installed in a schema named extensions, and a pool on it.
func extensionSchemaDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), "CREATE SCHEMA extensions; CREATE EXTENSION citext WITH SCHEMA extensions")
	require.NoError(t, err)
	return url, pool
}

// diff --sql for a table that does not exist yet writes the file's own
// statements, so the script sets the search path the create path runs
// them under. Run as written on a fresh session, which searches only the
// default path, it creates the table with the extension's type.
func TestDiffSQLForAMissingTableResolvesExtensionSchemas(t *testing.T) {
	url, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)
	path := filepath.Join(t.TempDir(), "accounts.sql")
	require.NoError(t, os.WriteFile(path, []byte("CREATE TABLE accounts (id int PRIMARY KEY, email citext NOT NULL);"), 0600))
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = parser.Parse([]string{"diff", "--url", url, "--schema", schema, "--desired", path, "--sql", "--extension-schema", "extensions"})
	require.NoError(t, err)
	var out strings.Builder
	require.NoError(t, root.Diff.run(t.Context(), &out))
	assert.Contains(t, out.String(), fmt.Sprintf("SET search_path = %q, \"public\", \"extensions\";\n", schema))

	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	_, err = conn.Exec(t.Context(), out.String())
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "extensions.citext", live.Columns[1].Type)
}

// A plan reviewed with one set of extension schemas does not pass the
// fingerprint check when it is applied with another. The SQL for a table
// that does not exist yet is the same either way, but the other schema's
// citext is a different type, so the run refuses and creates nothing.
func TestPinnedPlanRefusesOtherExtensionSchemas(t *testing.T) {
	_, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)
	other := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE DOMAIN %s.citext AS text", other))
	require.NoError(t, err)
	ds, err := statement.ParseDesired("CREATE TABLE accounts (id int PRIMARY KEY, email citext NOT NULL)")
	require.NoError(t, err)

	reviewedWith, err := ds.WithExtensionSchemas("extensions")
	require.NoError(t, err)
	reviewed, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: reviewedWith})
	require.NoError(t, err)
	assert.Equal(t, []string{"extensions"}, reviewed.ExtensionSchemas)

	appliedWith, err := ds.WithExtensionSchemas(other)
	require.NoError(t, err)
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             appliedWith,
		ExpectedFingerprint: reviewed.Fingerprint,
	}, migrate.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
	assert.Equal(t, reviewed.Statements, res.Plan.Statements, "the SQL alone cannot tell the two plans apart")
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound, "a refused plan creates nothing")
}
