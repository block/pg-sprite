package hosted_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run the shipped CLI, not a second implementation of desired-state execution.
func (f *fixture) cli(t *testing.T, wantExit int, args ...string) map[string]any {
	t.Helper()
	binary := os.Getenv("SUPABASE_HOSTED_BIN")
	require.NotEmpty(t, binary, "set SUPABASE_HOSTED_BIN to a freshly built pg-sprite binary")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	output, err := cmd.Output()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "CLI could not execute")
		code = exit.ExitCode()
	}
	require.Equal(t, wantExit, code, "CLI stdout: %s; stderr: %s", output, diagnostic.String())
	var result map[string]any
	if len(args) > 0 && args[len(args)-1] == "--json" {
		require.NoError(t, json.Unmarshal(output, &result))
	}
	return result
}

func desiredFile(t *testing.T, sql string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "desired.sql")
	require.NoError(t, os.WriteFile(path, []byte(sql), 0600))
	return path
}

func (f *fixture) assertExportConverges(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	f.cli(t, 0, "pull", "--schema", "public", "--out", dir)
	result := f.cli(t, 0, "diff", "--desired", filepath.Join(dir, f.name+".sql"), "--json")
	require.Contains(t, result, "statements")
	assert.Empty(t, result["statements"])
}

// This complete CLI lifecycle uses project defaults, then manages RLS explicitly.
// It does not subscribe to Realtime or change the publication.
func TestHostedCLICreateExportAndRLS(t *testing.T) {
	f := newFixture(t)
	name := pgx.Identifier{f.name}.Sanitize()
	base := fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL
);`, name)
	path := desiredFile(t, base)
	f.cli(t, 0, "migrate", "--desired", path, "--dry-run", "--json")
	f.prepareTableCleanup(t)
	f.cli(t, 0, "migrate", "--desired", path, "--json")
	f.exec(t, "INSERT INTO "+f.table+" VALUES (1,$1,'first'),(2,$2,'second')", f.users[0].ID, f.users[1].ID)
	f.assertExportConverges(t)

	rls := base + fmt.Sprintf(`
ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON %s FOR SELECT TO authenticated
    USING (owner_id = auth.uid());`, name, name)
	path = desiredFile(t, rls)
	preview := f.cli(t, 2, "migrate", "--desired", path, "--dry-run", "--json")
	assert.Equal(t, "unsupported-statement", preview["reason"])
	assert.NotEmpty(t, preview["row_security_review"], "preview must describe the RLS changes for review")
	f.cli(t, 0, "migrate", "--desired", path, "--json")
	f.waitForAPI(t)
	f.assertAccess(t)
	before := f.snapshot(t)
	result := f.cli(t, 0, "migrate", "--desired", path, "--json")
	assert.Equal(t, "executed", result["outcome"])
	assert.Empty(t, result["executed_sql"])
	assert.Equal(t, before, f.snapshot(t))
	f.assertExportConverges(t)
	f.assertAccess(t)
}

func TestHostedCLIAddColumn(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL,
    archived boolean NOT NULL DEFAULT false
);`, pgx.Identifier{f.name}.Sanitize()))
	f.cli(t, 0, "migrate", "--desired", path, "--json")
	var rows int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+f.table+" WHERE NOT archived AND ((id=1 AND body='first') OR (id=2 AND body='second'))").Scan(&rows))
	assert.Equal(t, 2, rows)
	f.assertAccess(t)
	f.assertExportConverges(t)
}

func TestHostedCLIAddIndex(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	before := f.snapshot(t)
	name := pgx.Identifier{f.name}.Sanitize()
	index := pgx.Identifier{f.name + "_body"}.Sanitize()
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL
);
CREATE INDEX %s ON %s (body);`, name, index, name))
	f.cli(t, 0, "migrate", "--desired", path, "--json")
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT indisvalid FROM pg_index WHERE indexrelid=$1::regclass", "public."+index).Scan(&valid))
	assert.True(t, valid)
	assert.Equal(t, before, f.snapshot(t), "index addition preserves rows, columns, table identity, policies, and grants")
	f.assertAccess(t)
	f.assertExportConverges(t)
}
