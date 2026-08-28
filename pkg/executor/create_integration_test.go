package executor_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// createFixture is one schema on a real server with an absence proof
// minted for the named table — the inputs ExecuteCreate requires.
type createFixture struct {
	pool   *pgxpool.Pool
	schema string
	at     preflight.AbsentTarget
}

func newCreateFixture(t *testing.T, table string) createFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, table)
	require.NoError(t, err)
	return createFixture{pool: pool, schema: schema, at: at}
}

func desired(t *testing.T, sql string) statement.DesiredSchema {
	t.Helper()
	ds, err := statement.ParseDesired(sql)
	require.NoError(t, err)
	return ds
}

// relationKind returns the pg_class relkind of schema.name, or "" when no
// relation owns the name — the catalog oracle for what a create run left.
func relationKind(t *testing.T, pool *pgxpool.Pool, schema, name string) string {
	t.Helper()
	var relkind *string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT c.relkind::text
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`,
		schema, name).Scan(&relkind))
	if relkind == nil {
		return ""
	}
	return *relkind
}

// relationExists reports whether any relation owns schema.name, for
// assertions where only presence matters.
func relationExists(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (
		   SELECT FROM pg_class c
		     JOIN pg_namespace n ON n.oid = c.relnamespace
		    WHERE n.nspname = $1 AND c.relname = $2)`,
		schema, name).Scan(&exists))
	return exists
}

func TestExecuteCreateRunsTableAndIndexes(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE TABLE t (id int PRIMARY KEY, name text);
		CREATE INDEX t_name_idx ON t (name);
		CREATE UNIQUE INDEX t_id_name_idx ON t (id, name);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)

	assert.Equal(t, "r", relationKind(t, f.pool, f.schema, "t"))
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_name_idx"))
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_id_name_idx"))

	require.Len(t, rep.Steps, 3)
	assert.Contains(t, rep.Steps[0].SQL, "CREATE TABLE")
	for _, step := range rep.Steps {
		assert.Equal(t, executor.StepBrief, step.Kind)
		assert.GreaterOrEqual(t, step.Duration, time.Duration(0))
	}
}

// A desired file may state its index before its table — declarative input
// carries no ordering contract — but an index cannot be built before its
// table exists, so the executor orders the CREATE TABLE first.
func TestExecuteCreateOrdersTableBeforeIndexes(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE INDEX t_name_idx ON t (name);
		CREATE TABLE t (id int, name text);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)

	require.Len(t, rep.Steps, 2)
	assert.Contains(t, rep.Steps[0].SQL, "CREATE TABLE")
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_name_idx"))
}

// The absence proof is time-of-check: a create that takes the name after
// the check surfaces as the typed collision, and the caller re-diffs
// rather than assuming what the occupant looks like.
func TestExecuteCreateReportsCollisionAsTyped(t *testing.T) {
	f := newCreateFixture(t, "t")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (other int)", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t (id int)")
	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.Error(t, err)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 1, stepErr.Step)
	assert.ErrorIs(t, err, executor.ErrCreateCollision)
	assert.Equal(t, executor.CodeCreateCollision, executor.OutcomeCode(err))
	assert.Empty(t, rep.Steps)
}

// A failed step ends the run; the steps before it committed and remain,
// and the report covers exactly that prefix so the caller can disclose
// what already happened.
func TestExecuteCreateFailedStepKeepsCommittedPrefix(t *testing.T) {
	f := newCreateFixture(t, "t")
	// Two indexes under one name: the second build fails with the
	// duplicate-name SQLSTATE after the table and first index committed.
	ds := desired(t, `
		CREATE TABLE t (id int, name text);
		CREATE INDEX dup_idx ON t (id);
		CREATE INDEX dup_idx ON t (name);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.Error(t, err)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 3, stepErr.Step)
	assert.Equal(t, 3, stepErr.Total)
	assert.ErrorIs(t, err, executor.ErrCreateCollision)

	assert.True(t, relationExists(t, f.pool, f.schema, "t"))
	assert.True(t, relationExists(t, f.pool, f.schema, "dup_idx"))
	require.Len(t, rep.Steps, 2)

	// The committed prefix is the rerun contract: the absence check now
	// refuses, which is the declarative front door's signal to re-diff.
	_, err = preflight.CheckTableAbsent(t.Context(), f.pool, f.schema, "t")
	assert.ErrorIs(t, err, preflight.ErrRelationExists)
}

func TestExecuteCreateAdmissionRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{
			name:    "if not exists on the table",
			sql:     "CREATE TABLE IF NOT EXISTS t (id int)",
			wantErr: executor.ErrIfNotExistsUnsupported,
		},
		{
			name:    "if not exists on an index",
			sql:     "CREATE TABLE t (id int); CREATE INDEX IF NOT EXISTS t_idx ON t (id)",
			wantErr: executor.ErrIfNotExistsUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCreateFixture(t, "t")
			ds := desired(t, tt.sql)

			_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, tt.wantErr)
			assert.False(t, relationExists(t, f.pool, f.schema, "t"),
				"admission covers the whole set before the first step executes")
		})
	}
}

func TestExecuteCreateRefusesPartitionOf(t *testing.T) {
	f := newCreateFixture(t, "t_part")
	_, err := f.pool.Exec(t.Context(),
		fmt.Sprintf("CREATE TABLE %s.parent (id int) PARTITION BY RANGE (id)", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t_part PARTITION OF parent FOR VALUES FROM (1) TO (10)")
	_, err = executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrPartitionOfUnsupported)
	assert.Equal(t, executor.CodePartitionOfUnsupported, executor.OutcomeCode(err))
}

// A desired schema for one table can never run against a proof minted for
// another: the mismatch is an invariant breach, not a refusal.
func TestExecuteCreateRefusesProofTargetMismatch(t *testing.T) {
	f := newCreateFixture(t, "other")
	ds := desired(t, "CREATE TABLE t (id int)")

	_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"))
}
