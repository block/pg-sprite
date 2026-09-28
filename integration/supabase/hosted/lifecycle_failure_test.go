package hosted_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Failed NOT NULL validation retains NULL rows and reports its committed scaffold.
func TestHostedCLIFailedNotNull(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	f.exec(t, "ALTER TABLE "+f.table+" ADD COLUMN label text")
	before := f.snapshot(t)
	result := f.cli(t, 1, "migrate", "--alter", "ALTER TABLE "+f.table+" ALTER COLUMN label SET NOT NULL", "--json")
	assert.Equal(t, "failed", result["outcome"])
	assert.Equal(t, "execution-failed", result["code"])
	require.Len(t, result["executed_sql"], 1, "the NOT VALID scaffold commits before validation fails")
	var nulls, scaffolds int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+f.table+" WHERE label IS NULL").Scan(&nulls))
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_constraint
 WHERE conrelid=$1::regclass AND contype='c' AND NOT convalidated`, f.table).Scan(&scaffolds))
	assert.Equal(t, 2, nulls)
	assert.Equal(t, 1, scaffolds)
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
}

// A destructive desired schema is rejected before dropping data or policies.
func TestHostedCLIRefuseDropColumn(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	before := f.snapshot(t)
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL
);`, pgx.Identifier{f.name}.Sanitize()))
	f.cli(t, 2, "migrate", "--desired", path, "--dry-run", "--json")
	f.cli(t, 2, "migrate", "--desired", path, "--json")
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
}

// A refused copy-and-swap plan cannot partially apply its safe column addition.
func TestHostedCLIRefuseCopySwap(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	before := f.snapshot(t)
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL,
    safe_prefix text,
    token uuid DEFAULT gen_random_uuid()
);`, pgx.Identifier{f.name}.Sanitize()))
	result := f.cli(t, 2, "migrate", "--desired", path, "--json")
	assert.Equal(t, "backend-unavailable", result["reason"])
	assert.Empty(t, result["verdicts"])
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
}

// A held lock prevents removing the last policy; access must remain unchanged.
func TestHostedCLIRLSLockFailure(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	before := f.snapshot(t)
	name := pgx.Identifier{f.name}.Sanitize()
	path := desiredFile(t, fmt.Sprintf(`CREATE TABLE %s (
    id integer PRIMARY KEY,
    owner_id uuid NOT NULL,
    body text NOT NULL
);
ALTER TABLE %s ENABLE ROW LEVEL SECURITY;`, name, name))
	holder, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_ = holder.Rollback(ctx)
	})
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+f.table+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	result := f.cli(t, 1, "migrate", "--desired", path, "--lock-timeout", "200ms", "--json")
	assert.Equal(t, "budget-lock-exceeded", result["code"])
	require.NoError(t, holder.Rollback(t.Context()))
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
}
