package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
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

func TestDiffRowSecurityRefusesMissingTableWithoutCreatingIt(t *testing.T) {
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
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file}
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	assert.NotEmpty(t, out.String(), "refusals must be visible to operators")
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
	assert.Equal(t, 2, envelope.Review.Version)
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
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	assert.Equal(t, live, model)
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
