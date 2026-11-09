package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestPullAndDiffWithRowSecurityRoundTrip(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %s.documents FOR SELECT TO PUBLIC USING (owner_id = 7);`, schema, schema, schema))
	require.NoError(t, err)
	var out strings.Builder
	dir := t.TempDir()
	pull := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: dir}
	require.NoError(t, pull.run(t.Context(), &out))
	file := filepath.Join(dir, "documents.sql")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `ENABLE ROW LEVEL SECURITY`)
	assert.Contains(t, string(raw), `CREATE POLICY "readers"`)
	out.Reset()
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	require.NoError(t, diff.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, "documents", report.Table)
	require.NotNil(t, report.TableExists)
	assert.True(t, *report.TableExists)
	assert.Empty(t, report.Statements)
}

// A desired file with row security for an absent table previews the create
// that migrate --desired would run, in every output format, and leaves the
// table absent.
func TestDiffRowSecurityPreviewsCreateOfMissingTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`), 0600))

	var out strings.Builder
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	require.NoError(t, cmd.run(t.Context(), &out))
	var preview struct {
		Schema      string   `json:"schema"`
		Table       string   `json:"table"`
		Statements  []string `json:"statements"`
		TableExists *bool    `json:"table_exists"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &preview))
	assert.Equal(t, schema, preview.Schema)
	assert.Equal(t, "documents", preview.Table)
	require.NotNil(t, preview.TableExists)
	assert.False(t, *preview.TableExists)
	require.Len(t, preview.Statements, 3)
	assert.Contains(t, preview.Statements[0], "CREATE TABLE "+schema+".documents")
	assert.Contains(t, preview.Statements[1], "ENABLE ROW LEVEL SECURITY")
	assert.Contains(t, preview.Statements[2], "NO FORCE ROW LEVEL SECURITY")

	out.Reset()
	cmd = &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, SQL: true}
	require.NoError(t, cmd.run(t.Context(), &out))
	script := out.String()
	assert.Equal(t, []string{
		"-- " + schema + ".documents does not exist; pg-sprite migrate --desired creates it with its row security in one transaction",
		"BEGIN;",
		fmt.Sprintf(`SET LOCAL search_path = "%s", "public";`, schema),
		preview.Statements[0] + ";",
		`SET LOCAL search_path = "pg_catalog";`,
		preview.Statements[1] + ";",
		preview.Statements[2] + ";",
		"COMMIT;",
	}, strings.Split(strings.TrimSpace(script), "\n"),
		"the script creates the table and its RLS in one transaction, each under the search_path migrate --desired uses")

	out.Reset()
	cmd = &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file}
	require.NoError(t, cmd.run(t.Context(), &out))
	assert.Contains(t, out.String(), schema+".documents does not exist. migrate --desired creates it with its row security in one transaction:")
	assert.Contains(t, out.String(), "  "+preview.Statements[0]+";")

	_, err = schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)

	_, err = pool.Exec(t.Context(), script)
	require.NoError(t, err, "the script runs as written")
	created, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, created.RowSecurity.Enabled)
}

// The --sql script for a missing table names the target schema's own types
// as the desired file does, unqualified. It sets the search_path
// migrate --desired creates the table under, so an operator who runs it from
// a session whose path does not include that schema creates the same table.
func TestDiffRowSecuritySQLScriptResolvesTheTargetSchemasTypes(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE DOMAIN %s.document_owner AS text CHECK (VALUE <> '')", schema))
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner document_owner NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`), 0600))

	var out strings.Builder
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, SQL: true}
	require.NoError(t, cmd.run(t.Context(), &out))
	_, err = pool.Exec(t.Context(), out.String())
	require.NoError(t, err, "the script resolves document_owner without the session's search_path")
	created, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	types := make(map[string]string)
	for _, col := range created.Columns {
		types[col.Name] = col.Type
	}
	assert.Equal(t, map[string]string{"id": "bigint", "owner": "document_owner"}, types)
	assert.True(t, created.RowSecurity.Enabled)
}

// A table created after diff found it missing, but before the create
// preview ran, is previewed as the existing table it now is. Calling the
// create preview on a table that already exists is that race, without its
// timing: it prints what diff prints for the existing table, here the
// review refusal for the row security it lacks.
func TestDiffRowSecurityPreviewsATableCreatedAfterPlanning(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.documents (id bigint PRIMARY KEY)", schema))
	require.NoError(t, err)
	sql := `CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(sql), 0600))
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)

	var existing strings.Builder
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	existingErr := cmd.run(t.Context(), &existing)
	require.ErrorIs(t, existingErr, verdict.ErrRefused)

	var raced strings.Builder
	racedErr := cmd.previewCreateWithRowSecurity(t.Context(), &raced, pool, desired,
		executor.Budget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second})
	require.ErrorIs(t, racedErr, verdict.ErrRefused)
	assert.Equal(t, existingErr.Error(), racedErr.Error())
	assert.JSONEq(t, existing.String(), raced.String())
	assert.Contains(t, raced.String(), `"kind": "enabled"`, "the review names the row security the existing table lacks")
}

func TestDiffRowSecurityRefusesDisableWithoutChangingLiveState(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (id bigint PRIMARY KEY);
 ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;`, schema, schema))
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`), 0600))
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var envelope struct {
		verdict.Verdict
		Review schemadiff.RowSecurityReview `json:"row_security_review"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &envelope))
	assert.Equal(t, verdict.OutcomeRefused, envelope.Outcome)
	assert.Equal(t, 1, envelope.Review.Version)
	require.Len(t, envelope.Review.Changes, 1)
	assert.Equal(t, schemadiff.AccessMayWiden, envelope.Review.Changes[0].Impact)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
}

func TestPullWithoutRowSecurityKeepsPlainTableFile(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
     id bigint PRIMARY KEY
 )`, schema))
	require.NoError(t, err)
	dir := t.TempDir()
	var out strings.Builder
	pull := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: dir}
	require.NoError(t, pull.run(t.Context(), &out))
	raw, err := os.ReadFile(filepath.Join(dir, "documents.sql"))
	require.NoError(t, err)
	desired, err := statement.ParseDesired(string(raw))
	require.NoError(t, err, "ordinary exports must remain valid table-only desired files")
	assert.Equal(t, "documents", desired.Table())
	assert.NotContains(t, string(raw), "ROW LEVEL SECURITY")
}

func TestDiffPolicyWithoutExplicitSettingIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 CREATE POLICY readers ON documents FOR SELECT USING (true);`), 0600))
	cmd := &DiffCmd{Desired: file}
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, statement.ErrRowSecurityDeclaration)
	require.NotErrorIs(t, err, verdict.ErrRefused)
	assert.Empty(t, out.String())
}

func TestPullPreservesEnabledRLSWithoutPolicies(t *testing.T) {
	verifyPulledRowSecurity(t, `ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;`)
}

func TestPullPreservesDisabledRLSWithPolicies(t *testing.T) {
	verifyPulledRowSecurity(t, `CREATE POLICY readers ON %[1]s.documents
     FOR SELECT TO PUBLIC USING (id > 0);`)
}

func TestPullPreservesForceWhileRLSIsDisabled(t *testing.T) {
	verifyPulledRowSecurity(t, `ALTER TABLE %[1]s.documents FORCE ROW LEVEL SECURITY;`)
}

func verifyPulledRowSecurity(t *testing.T, securitySQL string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
     id bigint PRIMARY KEY
 )`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(securitySQL, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	dir := t.TempDir()
	var out strings.Builder
	pull := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: dir}
	require.NoError(t, pull.run(t.Context(), &out))
	file := filepath.Join(dir, "documents.sql")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(string(raw))
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	assert.Equal(t, live, modeltest.Comparable(model))
	out.Reset()
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, JSON: true}
	require.NoError(t, diff.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Empty(t, report.Statements)
}

func TestDiffRowSecuritySQLCarriageReturnIdentifier(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	name := "documents\rSELECT 42; --"
	target := pgx.Identifier{schema, name}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s (
     id bigint PRIMARY KEY
 );`, target))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), "ALTER TABLE "+target+" ENABLE ROW LEVEL SECURITY")
	require.NoError(t, err)
	identifier := pgx.Identifier{name}.Sanitize()
	sql := fmt.Sprintf(`CREATE TABLE %[1]s (
     id bigint PRIMARY KEY
 );
 ALTER TABLE %[1]s DISABLE ROW LEVEL SECURITY;`, identifier)
	file := filepath.Join(t.TempDir(), "documents.sql")
	require.NoError(t, os.WriteFile(file, []byte(sql), 0600))
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, SQL: true}
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	_, err = statement.ParseDesired(out.String())
	require.ErrorIs(t, err, statement.ErrEmptyDesired, "refused SQL output must parse as zero statements")
	live, err := schemadiff.Introspect(t.Context(), pool, schema, name)
	require.NoError(t, err)
	assert.True(t, live.RowSecurity.Enabled)
}
