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

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
)

// sqlstateCheckViolation is the typed outcome a failed VALIDATE surfaces.
const sqlstateCheckViolation = "23514"

// runBudget bounds integration sequences: brief steps must prove themselves
// fast, the long classes get room to finish on tiny test tables.
// Validate.LockTimeout is deliberately distinct from Brief.LockTimeout: the
// budget-class tests prove which class a step ran under by the budget value
// its cancellation reports, so equal values would make a misclassification
// invisible.
var runBudget = executor.SequenceBudget{
	Brief:      executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 2 * time.Second},
	Concurrent: executor.ConcurrentBudget{Overall: time.Minute},
	Validate:   executor.ValidateBudget{LockTimeout: time.Second, Overall: time.Minute},
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

	// TM-1: the documented retry contract must actually work — fix the
	// violating data and resume from the failed step, using nothing but
	// the typed error's own step number.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("UPDATE %s.t SET v = 1 WHERE v <= 0", schema))
	require.NoError(t, err)
	rep, err := executor.RunSequence(t.Context(), pool, pt, steps[stepErr.Step-1:], runBudget)
	require.NoError(t, err, "resuming from the failed step must complete the sequence")
	require.Len(t, rep.Steps, 1)
	assert.Equal(t, executor.StepValidateConstraint, rep.Steps[0].Kind)
	_, validated = constraintState(t, pool, schema, "t", "v_positive")
	assert.True(t, validated, "the resumed validation must leave the constraint validated")
}

func TestRunSequenceBudgetCancelsBlockedBriefStep(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v int)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// A second session holds ACCESS EXCLUSIVE while the sequence runs, so
	// the brief step can never be granted its lock and the lock budget
	// fires. The cleanup rollback is a redundant safety closer: the test
	// body rolls the blocker back itself, and the guaranteed ErrTxClosed
	// from the cleanup is discarded.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = blocker.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	steps := []string{fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN v", schema)}
	_, err = executor.RunSequence(t.Context(), pool, pt, steps, runBudget)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 1, stepErr.Step)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr, "the budget outcome must stay reachable through the step error")
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, runBudget.Brief.LockTimeout, budgetErr.Budget, "a brief step must be cancelled by the brief lock budget")

	// TM-3: the cancelled step must have left durable state untouched —
	// the budget cancellation rolls back, it never half-commits.
	assert.True(t, columnExists(t, pool, schema, "t", "v"),
		"the lock-cancelled DROP COLUMN must not have committed")

	// TM-3: after the fault clears, the same sequence must proceed to
	// completion.
	require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	_, err = executor.RunSequence(t.Context(), pool, pt, steps, runBudget)
	require.NoError(t, err, "the sequence must complete once the lock holder is gone")
	assert.False(t, columnExists(t, pool, schema, "t", "v"))
}

// columnExists reports whether the named live column exists on the table —
// the durable-state oracle for cancelled brief steps.
func columnExists(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (
		   SELECT FROM pg_attribute
		    WHERE attrelid = to_regclass($1) AND attname = $2
		      AND attnum > 0 AND NOT attisdropped)`,
		schema+"."+table, column).Scan(&exists))
	return exists
}

// TestRunSequenceValidateRunsUnderValidateBudget proves the headline
// behavior of the validate class: a lone VALIDATE CONSTRAINT runs under
// ValidateBudget, not the brief budgets. A second-session ACCESS EXCLUSIVE
// holder parks the validate in the lock queue, and the budget value its
// cancellation reports — distinct from Brief.LockTimeout by fixture
// construction — proves which class it ran under.
func TestRunSequenceValidateRunsUnderValidateBudget(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0) NOT VALID",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	_, err = executor.RunSequence(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT v_positive", schema)}, runBudget)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, executor.StepValidateConstraint, stepErr.Kind)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, runBudget.Validate.LockTimeout, budgetErr.Budget,
		"the validate step must be cancelled by the validate lock budget, not the brief one")
}

// TestRunSequenceOperatorCancelOfValidateIsNotBudgetExhaustion covers the
// 57014 disambiguation for the validate class: an operator's
// pg_cancel_backend early in a generous validate budget must surface as
// ErrCancelledExternally, never as a *BudgetError — a consumer reading
// budget exhaustion would escalate to a heavier strategy when a human
// deliberately stopped the validation.
func TestRunSequenceOperatorCancelOfValidateIsNotBudgetExhaustion(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0) NOT VALID",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// An ACCESS EXCLUSIVE holder parks the validate in the lock queue,
	// giving the cancel a window; the generous budgets guarantee neither
	// timeout can fire first.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	b := runBudget
	b.Validate = executor.ValidateBudget{LockTimeout: time.Minute, Overall: time.Minute}
	done := make(chan error, 1)
	go func() {
		_, err := executor.RunSequence(t.Context(), pool, pt,
			[]string{fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT v_positive", schema)}, b)
		done <- err
	}()

	// Cancel the validate's backend once it is provably executing; the
	// ALTER TABLE prefix cannot match this polling query itself.
	require.Eventually(t, func() bool {
		var cancelled bool
		err := pool.QueryRow(t.Context(),
			`SELECT pg_cancel_backend(pid) FROM pg_stat_activity
			  WHERE query LIKE 'ALTER TABLE %' AND query LIKE '%VALIDATE CONSTRAINT%' AND state = 'active'`).Scan(&cancelled)
		return err == nil && cancelled
	}, 30*time.Second, 50*time.Millisecond, "the validate's backend must be found and cancelled")

	select {
	case err := <-done:
		require.ErrorIs(t, err, executor.ErrCancelledExternally)
		var stepErr *executor.SequenceStepError
		require.ErrorAs(t, err, &stepErr, "the cancel must still be attributed to its step")
		assert.Equal(t, executor.StepValidateConstraint, stepErr.Kind)
		var budgetErr *executor.BudgetError
		assert.False(t, errors.As(err, &budgetErr), "an operator cancel must not read as budget exhaustion")
	case <-time.After(time.Minute):
		t.Fatal("the cancelled sequence did not return")
	}
}

// TestRunSequenceSurfacesFailedConcurrentBuildStep makes a delegated build
// step fail inside a sequence: the *InvalidIndexError verdict must stay
// reachable through the step error, and the committed prefix must remain
// per the partial-failure contract.
func TestRunSequenceSurfacesFailedConcurrentBuildStep(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, v int); INSERT INTO %s.t VALUES (1, 7), (2, 7)",
		schema, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// The duplicate rows make the unique build fail after the brief step
	// committed.
	steps := []string{
		fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0) NOT VALID", schema),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY i_v ON %s.t (v)", schema),
	}
	_, err = executor.RunSequence(t.Context(), pool, pt, steps, runBudget)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 2, stepErr.Step)
	assert.Equal(t, executor.StepConcurrentIndexBuild, stepErr.Kind)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr, "the build's typed verdict must stay reachable through the step error")
	exists, _ := constraintState(t, pool, schema, "t", "v_positive")
	assert.True(t, exists, "the committed prefix must remain, per the partial-failure contract")
}

// TestRunSequenceRefusesSingleConnectionPoolBeforeAnyStep covers the
// admission-time pool guard for sequences containing a concurrent build:
// a pool refusal decidable up front must precede the first step, never
// fire mid-run after a committed prefix.
func TestRunSequenceRefusesSingleConnectionPoolBeforeAnyStep(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v int)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	// The brief step comes before the build: if the pool guard fired only
	// inside the delegated executor, the constraint would already be
	// committed.
	steps := []string{
		fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0) NOT VALID", schema),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY i_v ON %s.t (v)", schema),
	}
	_, err = executor.RunSequence(t.Context(), pool, pt, steps, runBudget)

	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	var stepErr *executor.SequenceStepError
	assert.False(t, errors.As(err, &stepErr), "the pool refusal must precede any step execution")
	exists, _ := constraintState(t, pool, schema, "t", "v_positive")
	assert.False(t, exists, "nothing may execute when the sequence cannot finish")
}
