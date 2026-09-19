package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/schemadiff"
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
	defaultPull := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: t.TempDir()}
	require.ErrorIs(t, defaultPull.run(t.Context(), &out), verdict.ErrRefused)
	out.Reset()
	dir := t.TempDir()
	pull := &PullCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Out: dir, RowSecurity: true}
	require.NoError(t, pull.run(t.Context(), &out))
	file := filepath.Join(dir, "documents.sql")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `ENABLE ROW LEVEL SECURITY`)
	assert.Contains(t, string(raw), `CREATE POLICY "readers"`)
	out.Reset()
	diff := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, RowSecurity: true, JSON: true}
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
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, RowSecurity: true}
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.ErrorIs(t, err, verdict.ErrRefused)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
	assert.Empty(t, out.String(), "no partial executable report")
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
	cmd := &DiffCmd{DBFlags: DBFlags{URL: url}, Schema: schema, Desired: file, RowSecurity: true}
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
}
