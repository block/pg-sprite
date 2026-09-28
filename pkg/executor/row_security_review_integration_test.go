package executor_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewedRowSecurityPreviewAndApply(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
   id bigint PRIMARY KEY,
   owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);
 `)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	plan, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	assert.Equal(t, schema, plan.Schema)
	assert.Equal(t, "documents", plan.Table)
	require.NotEmpty(t, plan.Statements)
	afterPreview, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, afterPreview, "preview must not change the target")
	report, err := executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, plan.Statements, budget)
	require.NoError(t, err)
	assert.Equal(t, plan.Statements, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, "(owner_id = 9)", *after.RowSecurity.Policies[0].Using)
	// A previous nonempty review does not authorize a different (now empty) sequence.
	_, err = executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, plan.Statements, budget)
	require.ErrorIs(t, err, executor.ErrRowSecurityPlanChanged)
	empty, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	assert.Empty(t, empty.Statements)
	report, err = executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, nil, budget)
	require.NoError(t, err)
	assert.Empty(t, report.Statements)
}

func TestReviewedRowSecurityRefusesDifferentSequences(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
   id bigint PRIMARY KEY,
   owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);
 `)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}
	plan, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	reversed := slices.Clone(plan.Statements)
	slices.Reverse(reversed)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	cases := map[string][]string{
		"empty is not a bypass":           nil,
		"reordered":                       reversed,
		"missing":                         plan.Statements[1:],
		"extra":                           append(slices.Clone(plan.Statements), plan.Statements[0]),
		"arbitrary SQL is never executed": {"DROP TABLE " + pgx.Identifier{schema, "documents"}.Sanitize()},
	}
	for name, reviewed := range cases {
		t.Run(name, func(t *testing.T) {
			report, err := executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, reviewed, budget)
			require.ErrorIs(t, err, executor.ErrRowSecurityPlanChanged)
			assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
			assert.Equal(t, executor.RowSecurityReport{}, report)
			after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestReviewedRowSecurityRefusesNewPolicyAfterPreview(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
   id bigint PRIMARY KEY,
   owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 `)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}
	plan, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
 CREATE POLICY writers ON %s FOR INSERT WITH CHECK (owner_id = 7);
 `, pgx.Identifier{schema, "documents"}.Sanitize()))
	require.NoError(t, err)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	report, err := executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, plan.Statements, budget)
	require.ErrorIs(t, err, executor.ErrRowSecurityPlanChanged)
	assert.Equal(t, executor.RowSecurityReport{}, report)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after, "keep the other actor's change intact")
}

func TestReviewedRowSecurityRechecksAfterLockWait(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
   id bigint PRIMARY KEY,
   owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 `)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: 3 * time.Second, StatementTimeout: 5 * time.Second}
	plan, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	target := pgx.Identifier{schema, "documents"}.Sanitize()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+target+" IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var worker sync.WaitGroup
	worker.Go(func() {
		_, err := executor.ExecuteReviewedRowSecurity(ctx, pool, schema, desired, plan.Statements, budget)
		done <- err
	})
	defer func() { cancel(); worker.Wait() }()
	const queueDeadline = 2 * time.Second
	const queuePoll = 10 * time.Millisecond
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS (
   SELECT 1 FROM pg_locks WHERE relation = $1::regclass
   AND mode = 'AccessExclusiveLock' AND NOT granted
  )`, target).Scan(&waiting)
		return err == nil && waiting
	}, queueDeadline, queuePoll)
	_, err = holder.Exec(t.Context(), "ALTER POLICY readers ON "+target+" RENAME TO renamed_readers")
	require.NoError(t, err)
	require.NoError(t, holder.Commit(t.Context()))
	require.ErrorIs(t, <-done, executor.ErrRowSecurityPlanChanged)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
	require.Len(t, after.RowSecurity.Policies, 1)
	assert.Equal(t, "renamed_readers", after.RowSecurity.Policies[0].Name)
	assert.Equal(t, "(owner_id = 7)", *after.RowSecurity.Policies[0].Using)
}

func TestReviewedRowSecurityBindsSQLNotEarlierPredicate(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
   id bigint PRIMARY KEY,
   owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);
 `)
	require.NoError(t, err)
	budget := executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}
	plan, err := executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), "ALTER POLICY readers ON "+pgx.Identifier{schema, "documents"}.Sanitize()+" USING (owner_id = 8)")
	require.NoError(t, err)
	// Replacement still drops the same policy and creates the same desired policy.
	// The SQL review contract deliberately does not bind the previous predicate.
	report, err := executor.ExecuteReviewedRowSecurity(t.Context(), pool, schema, desired, plan.Statements, budget)
	require.NoError(t, err)
	assert.Equal(t, plan.Statements, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, "(owner_id = 9)", *after.RowSecurity.Policies[0].Using)
}
