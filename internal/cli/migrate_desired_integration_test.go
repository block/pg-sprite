package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func desiredFixture(t *testing.T) (string, string, *pgxpool.Pool) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );`, schema))
	require.NoError(t, err)
	return url, schema, pool
}

func TestMigrateDesiredRLSAppliesAndConverges(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)
	var out strings.Builder
	cmd.DryRun = true
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.False(t, before.RowSecurity.Enabled, "preview must not change access")
	cmd.DryRun = false
	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	require.Len(t, v.ExecutedSQL, 3)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
	require.Len(t, after.RowSecurity.Policies, 1)
	assert.Equal(t, "readers", after.RowSecurity.Policies[0].Name)
	assert.Equal(t, "(owner_id = 7)", *after.RowSecurity.Policies[0].Using)
	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	v = verdict.Verdict{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Empty(t, v.ExecutedSQL)
}

func TestMigrateDesiredRLSRefusesMixedChanges(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL,
 body text
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, string(executor.CodeRowSecurityRefused), v.Code)
	assert.Empty(t, v.ExecutedSQL)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestMigrateDesiredTableAddsColumn(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %s.documents FOR SELECT USING (owner_id = 7);`, schema, schema))
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL,
 body text
 );`)
	var out strings.Builder
	cmd.DryRun = true
	require.NoError(t, cmd.run(t.Context(), &out))
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Len(t, before.Columns, 2)
	cmd.DryRun = false
	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	require.Len(t, result.Verdicts, 1)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, after.Columns, 3)
	assert.Equal(t, "body", after.Columns[2].Name)
	assert.Equal(t, before.RowSecurity, after.RowSecurity, "table-only files leave policies separately managed")
}

func TestMigrateDesiredTableRefusesDestructiveChange(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY
 );`)
	var out strings.Builder
	cmd.DryRun = true
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var preview plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &preview))
	assert.Equal(t, router.DispositionRefuse, preview.Disposition)
	require.Len(t, preview.Statements, 1)
	assert.Equal(t, verdict.ReasonDestructiveChange, preview.Statements[0].Reason)
	cmd.DryRun = false
	out.Reset()
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.ReasonDestructiveChange, result.Reason)
	assert.Empty(t, result.Verdicts)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Len(t, after.Columns, 2)
}

func TestMigrateDesiredCreatesMissingTable(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), "DROP TABLE "+schema+".documents")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );`)
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Len(t, after.Columns, 2)
	assert.Equal(t, "id", after.Columns[0].Name)
}
