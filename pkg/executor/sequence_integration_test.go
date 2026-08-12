package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
)

// sqlstateCheckViolation is the typed outcome a failed VALIDATE surfaces.
const sqlstateCheckViolation = "23514"

// runBudget bounds integration sequences: brief steps must prove themselves
// fast, the long classes get room to finish on tiny test tables.
var runBudget = executor.SequenceBudget{
	Brief:      executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 2 * time.Second},
	Concurrent: executor.ConcurrentBudget{Overall: time.Minute},
	Validate:   executor.ValidateBudget{LockTimeout: 500 * time.Millisecond, Overall: time.Minute},
}

// constraintState reports whether the named constraint exists on the table
// and whether it is validated — the catalog oracle for the NOT VALID
// pattern.
func constraintState(t *testing.T, pool *pgxpool.Pool, schema, table, constraint string) (exists, validated bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(),
		`SELECT con.convalidated
		   FROM pg_constraint con
		   JOIN pg_class c ON c.oid = con.conrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2 AND con.conname = $3`,
		schema, table, constraint).Scan(&validated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return true, validated
}

// saferSequence classifies sql through the planner and returns the safer
// sequence it constructed: the integration tests run exactly what the
// planner produces, proving the planner-to-executor seam end to end.
func saferSequence(t *testing.T, sql string) []string {
	t.Helper()
	plan, err := planner.Classify(sql, planner.Facts{})
	require.NoError(t, err)
	require.Len(t, plan.Decisions, 1)
	d := plan.Decisions[0]
	require.Equal(t, planner.ReasonSaferIdiom, d.Reason, "the test premise is a safer-idiom decision")
	require.NotEmpty(t, d.SaferSQL, "the planner must have constructed the sequence")
	require.Equal(t, planner.ExecutionAutocommit, d.SaferSQLExecution)
	return d.SaferSQL
}

func TestRunSequenceValidatesCheckConstraintOnline(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); INSERT INTO %s.t SELECT g, g FROM generate_series(1, 100) g",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	steps := saferSequence(t, fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0)", schema))
	rep, err := executor.RunSequence(t.Context(), pool, pt, steps, runBudget)
	require.NoError(t, err)

	require.Len(t, rep.Steps, 2)
	assert.Equal(t, executor.StepBrief, rep.Steps[0].Kind, "the NOT VALID add is a brief catalog step")
	assert.Equal(t, executor.StepValidateConstraint, rep.Steps[1].Kind, "the validation runs under its own class")
	exists, validated := constraintState(t, pool, schema, "t", "v_positive")
	assert.True(t, exists, "the constraint must exist")
	assert.True(t, validated, "the constraint must be validated, not left NOT VALID")
}

func TestRunSequenceSetNotNullLeavesNoScaffold(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); INSERT INTO %s.t SELECT g, g FROM generate_series(1, 100) g",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	steps := saferSequence(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema))
	rep, err := executor.RunSequence(t.Context(), pool, pt, steps, runBudget)
	require.NoError(t, err)
	require.Len(t, rep.Steps, 4)

	var notNull bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT attnotnull FROM pg_attribute
		  WHERE attrelid = to_regclass($1) AND attname = 'v'`,
		schema+".t").Scan(&notNull))
	assert.True(t, notNull, "the column must be NOT NULL")
	var scaffolds int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'c'`,
		schema+".t").Scan(&scaffolds))
	assert.Zero(t, scaffolds, "the proving CHECK scaffold must be dropped")
}

func TestRunSequenceAddsPrimaryKeyOverConcurrentBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int NOT NULL, v int); INSERT INTO %s.t SELECT g, g FROM generate_series(1, 100) g",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	steps := saferSequence(t, fmt.Sprintf("ALTER TABLE %s.t ADD PRIMARY KEY (id)", schema))
	rep, err := executor.RunSequence(t.Context(), pool, pt, steps, runBudget)
	require.NoError(t, err)

	require.Len(t, rep.Steps, 2)
	assert.Equal(t, executor.StepConcurrentIndexBuild, rep.Steps[0].Kind)
	require.NotNil(t, rep.Steps[0].Index, "the build step must carry the verified index report")
	assert.NotZero(t, rep.Steps[0].Index.IndexOID)
	assert.Equal(t, executor.StepBrief, rep.Steps[1].Kind, "the USING INDEX attach is a brief catalog step")

	var pkIndex string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT ci.relname
		   FROM pg_constraint con
		   JOIN pg_class ci ON ci.oid = con.conindid
		  WHERE con.conrelid = to_regclass($1) AND con.contype = 'p'`,
		schema+".t").Scan(&pkIndex))
	assert.Equal(t, rep.Steps[0].Index.Index, pkIndex, "the primary key must own the concurrently built index")
}

func TestRunSequenceRunsSingleStepChanges(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// A fast-default ADD COLUMN and a metadata-only change are one-step
	// sequences: the executor covers them without a dedicated path.
	rep, err := executor.RunSequence(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int NOT NULL DEFAULT 0", schema)}, runBudget)
	require.NoError(t, err)
	require.Len(t, rep.Steps, 1)
	assert.Equal(t, executor.StepBrief, rep.Steps[0].Kind)
	assert.Equal(t, "integer", columnType(t, pool, schema, "t", "age"))

	rep, err = executor.RunSequence(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN age DROP DEFAULT", schema)}, runBudget)
	require.NoError(t, err)
	require.Len(t, rep.Steps, 1)
}

func TestRunSequenceStopsAtFailingStepAndReportsPartialState(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); INSERT INTO %s.t VALUES (1, -1)",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// The violating row makes step 1 (the NOT VALID add) succeed and step 2
	// (the validation scan) fail: the documented partial state is the
	// constraint left NOT VALID.
	steps := saferSequence(t, fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0)", schema))
	_, err = executor.RunSequence(t.Context(), pool, pt, steps, runBudget)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 2, stepErr.Step, "the validation step must be the one reported failed")
	assert.Equal(t, 2, stepErr.Total)
	assert.Equal(t, executor.StepValidateConstraint, stepErr.Kind)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server failure must stay reachable through the step error")
	assert.Equal(t, sqlstateCheckViolation, pgErr.Code)

	exists, validated := constraintState(t, pool, schema, "t", "v_positive")
	assert.True(t, exists, "the committed step's constraint must remain, per the partial-failure contract")
	assert.False(t, validated, "the failed validation must leave the constraint NOT VALID")
}

func TestRunSequenceBudgetCancelsBlockedBriefStep(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v int)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// A second session holds ACCESS EXCLUSIVE for the whole test, so the
	// brief step can never be granted its lock and the lock budget fires.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	_, err = executor.RunSequence(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN v", schema)}, runBudget)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 1, stepErr.Step)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr, "the budget outcome must stay reachable through the step error")
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
}
