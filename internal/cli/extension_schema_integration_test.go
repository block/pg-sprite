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
	"github.com/block/pg-sprite/pkg/executor"
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

// A file that declares row security takes --extension-schema for its table
// definition. migrate --desired creates the missing table with its row
// security and an extension column, a column added later converges as an
// ordinary desired run, and a policy change runs through the row security
// executor. A policy expression still resolves nothing but the table and
// PostgreSQL's built-ins, so a policy that names the extension's type
// unqualified is refused and nothing changes.
func TestMigrateDesiredRowSecurityResolvesExtensionSchema(t *testing.T) {
	url, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)
	file := func(columns, policy string) string {
		return fmt.Sprintf(`CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL%s
 );
 ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON accounts FOR SELECT USING (%s);`, columns, policy)
	}
	run := func(sql string, args ...string) (*MigrateCmd, string, error) {
		cmd := desiredCommand(t, url, schema, sql, append([]string{"--extension-schema", "extensions"}, args...)...)
		var out strings.Builder
		err := cmd.run(t.Context(), &out)
		return cmd, out.String(), err
	}

	_, out, err := run(file("", "id > 0"), "--dry-run")
	require.NoError(t, err)
	var preview executor.RowSecurityPlan
	require.NoError(t, json.Unmarshal([]byte(out), &preview))
	assert.Equal(t, []string{"extensions"}, preview.ExtensionSchemas)
	require.NotEmpty(t, preview.Statements)
	assert.Equal(t, "CREATE TABLE "+schema+".accounts (id int PRIMARY KEY, email citext NOT NULL)", preview.Statements[0])

	_, out, err = run(file("", "id > 0"))
	require.NoError(t, err)
	var created verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	assert.Equal(t, verdict.OutcomeExecuted, created.Outcome, created.Detail)
	assert.True(t, created.TableCreated)
	assert.Equal(t, []string{"extensions"}, created.ExtensionSchemas, "the create ran under the extension schemas")
	require.NotEmpty(t, created.ExecutedSQL)
	assert.Equal(t, "CREATE TABLE "+schema+".accounts (id int PRIMARY KEY, email citext NOT NULL)", created.ExecutedSQL[0])
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "extensions.citext", live.Columns[1].Type)
	require.Len(t, live.RowSecurity.Policies, 1)

	_, out, err = run(file(",\n alias citext", "id > 0"))
	require.NoError(t, err)
	var added migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out), &added))
	require.Equal(t, verdict.OutcomeExecuted, added.Outcome, added.Detail)
	assert.Equal(t, []string{"extensions"}, added.Plan.ExtensionSchemas)
	require.Len(t, added.Plan.Statements, 1)
	assert.Equal(t, "ALTER TABLE "+schema+".accounts ADD COLUMN alias extensions.citext", added.Plan.Statements[0].SQL)
	require.NotNil(t, added.Convergence)
	assert.Equal(t, migrate.ConvergenceVerified, added.Convergence.Status)

	_, out, err = run(file(",\n alias citext", "id > 7"))
	require.NoError(t, err)
	var narrowed verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out), &narrowed))
	assert.Equal(t, verdict.OutcomeExecuted, narrowed.Outcome, narrowed.Detail)
	require.NotEmpty(t, narrowed.ExecutedSQL)
	assert.Equal(t, `DROP POLICY "readers" ON "`+schema+`"."accounts"`, narrowed.ExecutedSQL[0])
	assert.Contains(t, narrowed.ExecutedSQL[len(narrowed.ExecutedSQL)-1], "USING ((id > 7))")
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, before.RowSecurity.Policies, 1)
	assert.Equal(t, "(id > 7)", *before.RowSecurity.Policies[0].Using)

	_, _, err = run(file(",\n alias citext", "email = 'a'::citext"))
	require.ErrorContains(t, err, `type "citext" does not exist`)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused policy changes nothing")
}

// A policy that names an extension's operator and type qualified passes
// the row security path, and keeps that meaning when another extension
// schema on the table definition's path offers a competing operator of the
// same name: the policy depends on the extension's operator, a rerun finds
// nothing to change, and a role the policy applies to sees exactly the
// rows the case-insensitive comparison admits.
func TestMigrateDesiredRowSecurityKeepsAQualifiedExtensionOperator(t *testing.T) {
	url, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), `
		CREATE SCHEMA competing;
		CREATE FUNCTION competing.always(extensions.citext, extensions.citext) RETURNS boolean
			LANGUAGE sql IMMUTABLE AS 'SELECT true';
		CREATE OPERATOR competing.= (LEFTARG = extensions.citext, RIGHTARG = extensions.citext, FUNCTION = competing.always)`)
	require.NoError(t, err)
	const file = `CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL
 );
 ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON accounts FOR SELECT USING (email OPERATOR(extensions.=) 'alice'::extensions.citext);`
	run := func() string {
		cmd := desiredCommand(t, url, schema, file, "--extension-schema", "extensions", "--extension-schema", "competing")
		var out strings.Builder
		require.NoError(t, cmd.run(t.Context(), &out))
		return out.String()
	}

	var created verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(run()), &created))
	require.Equal(t, verdict.OutcomeExecuted, created.Outcome, created.Detail)
	assert.True(t, created.TableCreated)
	assert.Equal(t, []string{"extensions", "competing"}, created.ExtensionSchemas)

	rows, err := pool.Query(t.Context(), `
		SELECT n.nspname::pg_catalog.text
		FROM pg_catalog.pg_policy p
		JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_policy'::pg_catalog.regclass AND d.objid = p.oid
		JOIN pg_catalog.pg_operator o ON d.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND o.oid = d.refobjid
		JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
		WHERE p.polrelid = pg_catalog.to_regclass($1)`, pgx.Identifier{schema, "accounts"}.Sanitize())
	require.NoError(t, err)
	operators, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"extensions"}, operators, "the policy depends on the extension's operator, not the competing one")

	before, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	run()
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	assert.Equal(t, before, after, "a rerun changes nothing")

	reader := testutil.NewRole(t, pool, "")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %[1]s.accounts VALUES (1, 'ALICE'), (2, 'bob');
		GRANT USAGE ON SCHEMA %[1]s, extensions TO %[2]s;
		GRANT SELECT ON %[1]s.accounts TO %[2]s`, pgx.Identifier{schema}.Sanitize(), pgx.Identifier{reader}.Sanitize()))
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })
	_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{reader}.Sanitize())
	require.NoError(t, err)
	visible, err := tx.Query(t.Context(), fmt.Sprintf("SELECT email::pg_catalog.text FROM %s.accounts ORDER BY id", pgx.Identifier{schema}.Sanitize()))
	require.NoError(t, err)
	emails, err := pgx.CollectRows(visible, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"ALICE"}, emails, "the extension's case-insensitive comparison decides which rows the policy admits")
}

// diff --sql for a missing table whose file declares row security sets the
// extension schemas on the table definition's search path and pg_catalog
// alone on the row security's. Run as written on a fresh session, it
// creates the table with the extension's type and its policy.
func TestDiffSQLForAMissingRowSecurityTableResolvesExtensionSchemas(t *testing.T) {
	url, pool := extensionSchemaDatabase(t)
	schema := testutil.NewSchema(t, pool)
	path := filepath.Join(t.TempDir(), "accounts.sql")
	require.NoError(t, os.WriteFile(path, []byte(`CREATE TABLE accounts (id int PRIMARY KEY, email citext NOT NULL);
 ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON accounts FOR SELECT USING (id > 0);`), 0600))
	root := New("test")
	parser, err := kong.New(root, kong.Vars{"version": "test"})
	require.NoError(t, err)
	_, err = parser.Parse([]string{"diff", "--url", url, "--schema", schema, "--desired", path, "--sql", "--extension-schema", "extensions"})
	require.NoError(t, err)
	var out strings.Builder
	require.NoError(t, root.Diff.run(t.Context(), &out))
	assert.Contains(t, out.String(), fmt.Sprintf("SET LOCAL search_path = %q, \"public\", \"extensions\";\n", schema))
	assert.Contains(t, out.String(), "SET LOCAL search_path = \"pg_catalog\";\n")

	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	_, err = conn.Exec(t.Context(), out.String())
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "extensions.citext", live.Columns[1].Type)
	require.Len(t, live.RowSecurity.Policies, 1)
	assert.True(t, live.RowSecurity.Enabled)
}
