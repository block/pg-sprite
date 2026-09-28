package hosted_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auth.uid() must resolve the real HTTP caller, not the administrator applying DDL.
func TestHostedCLIAuthDefault(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL DEFAULT auth.uid(),
    body text NOT NULL
);`, pgx.Identifier{f.name}.Sanitize()))
	f.cli(t, 0, "migrate", "--desired", path, "--json")
	f.waitForAPI(t)
	status, _, err := f.request(t.Context(), http.MethodPost, "/rest/v1/"+f.name, f.keys.Publishable, f.users[0].Token, map[string]any{"id": 3, "body": "default owner"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	var owner string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT owner_id::text FROM "+f.table+" WHERE id=3").Scan(&owner))
	assert.Equal(t, f.users[0].ID, owner)
	status, _, err = f.request(t.Context(), http.MethodPost, "/rest/v1/"+f.name, f.keys.Publishable, f.users[0].Token, map[string]any{"id": 4, "owner_id": f.users[1].ID, "body": "spoofed owner"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, status)
	f.assertRows(t, f.users[0].Token, 1, 3)
	f.assertRows(t, f.users[1].Token, 2)
	f.assertRows(t, "")
	f.assertExportConverges(t)
}

// Reference users created through Auth; never insert directly into managed auth tables.
func TestHostedCLIAuthForeignKey(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	before := f.snapshot(t)
	f.cli(t, 0, "migrate", "--alter", "ALTER TABLE "+f.table+" ADD CONSTRAINT owner_fk FOREIGN KEY (owner_id) REFERENCES auth.users (id)", "--json")
	var valid bool
	var target string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT convalidated, confrelid::regclass::text FROM pg_constraint WHERE conrelid=$1::regclass AND conname='owner_fk'`, f.table).Scan(&valid, &target))
	assert.True(t, valid)
	assert.Equal(t, "auth.users", target)
	assert.Equal(t, before, f.snapshot(t))
	var orphan string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT gen_random_uuid()::text").Scan(&orphan))
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM auth.users WHERE id=$1)", orphan).Scan(&exists))
	require.False(t, exists)
	_, err := f.pool.Exec(t.Context(), "INSERT INTO "+f.table+" VALUES (3,$1,'orphan')", orphan)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23503", pgErr.Code)
	f.assertAccess(t)
}

// A failed concurrent unique build reports its invalid index instead of hiding it.
func TestHostedCLIUniqueIndexFailure(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	f.exec(t, "UPDATE "+f.table+" SET body='duplicate'")
	before := f.snapshot(t)
	index := pgx.Identifier{f.name + "_unique"}.Sanitize()
	result := f.cli(t, 1, "migrate", "--alter", "CREATE UNIQUE INDEX "+index+" ON "+f.table+" (body)", "--json")
	assert.Equal(t, "failed", result["outcome"])
	assert.Equal(t, "invalid-index-own-leftover", result["code"])
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT indisvalid FROM pg_index WHERE indexrelid=$1::regclass", "public."+index).Scan(&valid))
	assert.False(t, valid)
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
}
