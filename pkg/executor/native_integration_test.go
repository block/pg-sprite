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
)

// buildBudget bounds test builds: generous enough that a healthy build on a
// tiny table always finishes, bounded so a wedged test cannot hang the run.
var buildBudget = executor.ConcurrentBudget{Overall: time.Minute}

// SQLSTATEs the tests assert on: typed outcomes, never message text.
const (
	sqlstateUniqueViolation = "23505"
	sqlstateDuplicateTable  = "42P07"
)

// indexState reports whether the index exists and whether it is valid.
func indexState(t *testing.T, pool *pgxpool.Pool, schema, index string) (exists, valid bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(),
		`SELECT i.indisvalid
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indexrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`,
		schema, index).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return true, valid
}

// leaveInvalidIndex produces the real debris of a failed concurrent build:
// a unique build over duplicate rows fails after creating its catalog
// entry, leaving the index invalid.
func leaveInvalidIndex(t *testing.T, pool *pgxpool.Pool, schema, table, index string) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s.%s (c)", index, schema, table))
	require.Error(t, err, "a unique build over duplicates must fail")
	exists, valid := indexState(t, pool, schema, index)
	require.True(t, exists, "the failed build must leave a catalog entry")
	require.False(t, valid, "the leftover must be invalid")
}

func TestBuildIndexConcurrentlyBuildsValidIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	rep, err := executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	assert.Equal(t, schema, rep.Schema)
	assert.Equal(t, "idx_c", rep.Index)
	assert.Positive(t, rep.Duration, "the report must carry the build's wall-clock time")
	assert.NotEmpty(t, rep.ServerVersion, "the report must carry the server version that ran the build")
	// The reported OID must be the built index's catalog identity.
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT c.oid FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`, schema, "idx_c").Scan(&oid))
	assert.Equal(t, oid, rep.IndexOID)
	exists, valid := indexState(t, pool, schema, "idx_c")
	assert.True(t, exists, "the index must exist")
	assert.True(t, valid, "the index must be valid")
}

func TestExecuteNativeSequenceAppliesSafeDDLIdioms(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.customers (id int PRIMARY KEY);
		CREATE TABLE %[1]s.orders (id int NOT NULL, customer_id int, amount int);
		INSERT INTO %[1]s.customers VALUES (1);
		INSERT INTO %[1]s.orders VALUES (1, 1, 10)`, schema))
	require.NoError(t, err)

	sql := []string{
		fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT amount_positive CHECK (amount > 0) NOT VALID", schema),
		fmt.Sprintf("ALTER TABLE %s.orders VALIDATE CONSTRAINT amount_positive", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_customer_fk FOREIGN KEY (customer_id) REFERENCES %s.customers (id) NOT VALID", schema, schema),
		fmt.Sprintf("ALTER TABLE %s.orders VALIDATE CONSTRAINT orders_customer_fk", schema),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY orders_pkey ON %s.orders (id)", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey", schema),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY orders_amount_key ON %s.orders (amount)", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_amount_key UNIQUE USING INDEX orders_amount_key", schema),
		fmt.Sprintf("CREATE INDEX CONCURRENTLY orders_customer_idx ON %s.orders (customer_id)", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ADD COLUMN region text DEFAULT 'emea'", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ALTER COLUMN region SET DEFAULT 'apac'", schema),
	}
	require.NoError(t, executor.ExecuteNativeSequence(t.Context(), pool, sql, budget, buildBudget))

	var validatedCheck, validatedFK, primaryKey, unique bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT bool_and(convalidated) FILTER (WHERE conname = 'amount_positive'),
		       bool_and(convalidated) FILTER (WHERE conname = 'orders_customer_fk'),
		       bool_or(contype = 'p'),
		       bool_or(contype = 'u')
		  FROM pg_constraint
		 WHERE conrelid = $1::regclass`, fmt.Sprintf("%s.orders", schema)).Scan(
		&validatedCheck, &validatedFK, &primaryKey, &unique))
	assert.True(t, validatedCheck)
	assert.True(t, validatedFK)
	assert.True(t, primaryKey)
	assert.True(t, unique, "the UNIQUE USING INDEX attachment must land")

	exists, valid := indexState(t, pool, schema, "orders_customer_idx")
	assert.True(t, exists, "the plain concurrent index must exist")
	assert.True(t, valid, "the plain concurrent index must be valid")

	var existing, defaultValue string
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT region, pg_get_expr(d.adbin, d.adrelid) FROM %s.orders o JOIN pg_attrdef d ON d.adrelid = '%s.orders'::regclass JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum WHERE a.attname = 'region'", schema, schema)).Scan(&existing, &defaultValue))
	assert.Equal(t, "emea", existing, "the fast default must be visible on the existing row")
	assert.Equal(t, "'apac'::text", defaultValue, "the metadata-only SET DEFAULT must update the catalog default")
}

// TestExecuteNativeSequenceRefusesNullablePrimaryKeyUsingIndex covers the
// execution-time catalog proof: attaching a PRIMARY KEY over a nullable key
// column would make PostgreSQL run an implicit full-table SET NOT NULL scan
// under ACCESS EXCLUSIVE, so the step is refused. It also proves the
// documented partial-progress contract: the earlier committed step — the
// concurrent index build — remains, visible to the caller.
func TestExecuteNativeSequenceRefusesNullablePrimaryKeyUsingIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.orders (id int, amount int); INSERT INTO %[1]s.orders VALUES (1, 10)", schema))
	require.NoError(t, err)

	sql := []string{
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY orders_pkey ON %s.orders (id)", schema),
		fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey", schema),
	}
	err = executor.ExecuteNativeSequence(t.Context(), pool, sql, budget, buildBudget)

	require.ErrorIs(t, err, executor.ErrBackingIndexNotProven)
	assert.ErrorContains(t, err, "execute native step 2")

	exists, valid := indexState(t, pool, schema, "orders_pkey")
	assert.True(t, exists, "the committed earlier step must remain")
	assert.True(t, valid, "the committed earlier step must remain valid")
	var primaryKey bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT COALESCE(bool_or(contype = 'p'), false) FROM pg_constraint WHERE conrelid = $1::regclass`,
		fmt.Sprintf("%s.orders", schema)).Scan(&primaryKey))
	assert.False(t, primaryKey, "the refused attachment must not have executed")
}

// TestExecuteNativeSequenceUsingIndexProofRunsUnderTableLock covers the
// proof-then-act coupling: the backing-index proof runs only after the
// attachment's ACCESS EXCLUSIVE lock is held, so a concurrent session can
// never invalidate the proof between inspection and execution. With another
// session holding the table lock, the step must surface the bounded lock
// budget — not a refusal computed from a snapshot the ALTER would not see.
func TestExecuteNativeSequenceUsingIndexProofRunsUnderTableLock(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.orders (id int, amount int); INSERT INTO %[1]s.orders VALUES (1, 10)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE UNIQUE INDEX CONCURRENTLY orders_pkey ON %s.orders (id)", schema))
	require.NoError(t, err)

	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.orders IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	// The nullable id column would be refused by the proof — but the proof
	// must not run until the lock is granted, so the bounded lock wait
	// fires first.
	sql := []string{fmt.Sprintf(
		"ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY USING INDEX orders_pkey", schema)}
	err = executor.ExecuteNativeSequence(t.Context(), pool, sql, budget, buildBudget)

	require.NotErrorIs(t, err, executor.ErrBackingIndexNotProven,
		"the proof must not read a snapshot the locked ALTER would not see")
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, budget.LockTimeout, budgetErr.Budget)
}

// TestExecuteNativeSequenceDirectStepLockBudget covers invariant LK-2 on
// the direct path: a direct step blocked on its lock is cancelled by the
// executor-applied lock_timeout and surfaces as a typed *BudgetError, no
// matter how the pool's session defaults are configured.
func TestExecuteNativeSequenceDirectStepLockBudget(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int)", schema))
	require.NoError(t, err)

	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	sql := []string{fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT c CHECK (id > 0) NOT VALID", schema)}
	err = executor.ExecuteNativeSequence(t.Context(), pool, sql, budget, buildBudget)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, budget.LockTimeout, budgetErr.Budget)
}

// TestExecuteNativeSequenceValidateRunsUnderOverallDeadline covers the LK-2
// exception policy on the validate path: a blocked VALIDATE CONSTRAINT is
// bounded by the overall deadline (CauseStatement), never cancelled by a
// per-lock timeout (CauseLock) — the wait policy that keeps a healthy
// long validation from being killed mid-wait.
func TestExecuteNativeSequenceValidateRunsUnderOverallDeadline(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int);
		ALTER TABLE %[1]s.t ADD CONSTRAINT c CHECK (id IS NOT NULL) NOT VALID`, schema))
	require.NoError(t, err)

	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	sql := []string{fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT c", schema)}
	tight := executor.ConcurrentBudget{Overall: 500 * time.Millisecond}
	err = executor.ExecuteNativeSequence(t.Context(), pool, sql, budget, tight)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseStatement, budgetErr.Cause, "the overall deadline must fire, not a lock timeout")
	assert.Equal(t, tight.Overall, budgetErr.Budget)
}

// TestBuildIndexConcurrentlyRefusesSingleConnectionPool covers the
// admission-time pool guard: the verdict session is a correctness
// dependency reserved alongside the build session, so a pool that cannot
// hold two connections is refused before anything executes.
func TestBuildIndexConcurrentlyRefusesSingleConnectionPool(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	exists, _ := indexState(t, pool, schema, "idx_c")
	assert.False(t, exists, "the refusal must precede any execution")
}

// TestBuildIndexConcurrentlyOperatorCancelIsNotBudgetExhaustion covers the
// 57014 disambiguation end to end: an operator's pg_cancel_backend seconds
// into a generous budget must surface as ErrCancelledExternally, never as
// a *BudgetError — a consumer reading budget exhaustion would escalate to
// a heavier strategy when a human deliberately stopped the build.
func TestBuildIndexConcurrentlyOperatorCancelIsNotBudgetExhaustion(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// A repeatable-read snapshot blocks the build in its wait phase, so
	// there is a window to cancel it from another session.
	blocker, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})

	done := make(chan error, 1)
	go func() {
		_, err := executor.BuildIndexConcurrently(t.Context(), pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_op ON %s.t (c)", schema), buildBudget)
		done <- err
	}()

	// Cancel the build's backend once it is provably executing.
	require.Eventually(t, func() bool {
		var cancelled bool
		err := pool.QueryRow(t.Context(),
			`SELECT pg_cancel_backend(pid) FROM pg_stat_activity
			  WHERE query LIKE 'CREATE INDEX CONCURRENTLY idx_op%' AND state = 'active'`).Scan(&cancelled)
		return err == nil && cancelled
	}, 30*time.Second, 50*time.Millisecond, "the build's backend must be found and cancelled")

	select {
	case err := <-done:
		require.ErrorIs(t, err, executor.ErrCancelledExternally)
		var budgetErr *executor.BudgetError
		assert.False(t, errors.As(err, &budgetErr), "an operator cancel must not read as budget exhaustion")
	case <-time.After(time.Minute):
		t.Fatal("the cancelled build did not return")
	}
}

func TestBuildIndexConcurrentlyMissingTable(t *testing.T) {
	pool, schema := newPool(t)
	_, err := executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx ON %s.missing_table (c)", schema), buildBudget)
	require.ErrorIs(t, err, executor.ErrTableNotFound)
}

func TestBuildIndexConcurrentlyFailedBuildReportsItsLeftover(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); INSERT INTO %s.t VALUES (1, 7), (2, 7)", schema, schema))
	require.NoError(t, err)

	// A unique build over duplicates fails after creating its catalog
	// entry. The executor never drops — it must report the leftover as a
	// typed outcome carrying both the build failure and the recovery need.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_dup ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, schema, invalidErr.Schema)
	assert.Equal(t, "idx_dup", invalidErr.Index)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, invalidErr.Build, &pgErr, "the build failure must ride inside the typed outcome")
	assert.Equal(t, sqlstateUniqueViolation, pgErr.Code, "the build failure must surface its SQLSTATE")
	exists, valid := indexState(t, pool, schema, "idx_dup")
	assert.True(t, exists, "the executor must not have dropped the leftover")
	assert.False(t, valid, "the leftover must be invalid")

	// The explicit recovery the error names, then a clean retry.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX CONCURRENTLY %s.idx_dup", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DELETE FROM %s.t WHERE id = 2", schema))
	require.NoError(t, err)
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_dup ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)
	exists, valid = indexState(t, pool, schema, "idx_dup")
	assert.True(t, exists)
	assert.True(t, valid)
}

func TestBuildIndexConcurrentlyFailsClosedOnPreexistingInvalidLeftover(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); INSERT INTO %s.t VALUES (1, 7), (2, 7)", schema, schema))
	require.NoError(t, err)
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")

	// A pre-existing invalid index cannot be proven this build's own
	// debris, so the executor must refuse to touch it and name the
	// explicit recovery — never build, never drop.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrPreexistingInvalidIndex)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, schema, invalidErr.Schema)
	assert.Equal(t, "idx_left", invalidErr.Index)
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists, "the pre-existing leftover must survive untouched")
	assert.False(t, valid, "the pre-existing leftover must keep its invalid state")

	// The explicit recovery the error names, then a clean rebuild.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX CONCURRENTLY %s.idx_left", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DELETE FROM %s.t WHERE id = 2", schema))
	require.NoError(t, err)

	rep, err := executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)
	assert.Equal(t, schema, rep.Schema)
	assert.Equal(t, "idx_left", rep.Index)
	assert.NotZero(t, rep.IndexOID)
	assert.Positive(t, rep.Duration)
	assert.NotEmpty(t, rep.ServerVersion)
	exists, valid = indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.True(t, valid)
}

func TestBuildIndexConcurrentlyNeverDropsUnrelatedTableLeftover(t *testing.T) {
	pool, schema := newPool(t)
	// Two tables in the same schema; table a carries the invalid debris of
	// a failed build under the exact name the new build on table b wants.
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		`CREATE TABLE %s.a (id int PRIMARY KEY, c int);
		 CREATE TABLE %s.b (id int PRIMARY KEY, c int);
		 INSERT INTO %s.a VALUES (1, 7), (2, 7)`, schema, schema, schema))
	require.NoError(t, err)
	leaveInvalidIndex(t, pool, schema, "a", "idx_shared")

	// The build on table b must refuse before touching anything: an
	// invalid index under this name anywhere in the schema cannot be
	// proven anyone's, and a's leftover must survive untouched for its own
	// table's recovery.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_shared ON %s.b (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrPreexistingInvalidIndex)
	exists, valid := indexState(t, pool, schema, "idx_shared")
	assert.True(t, exists, "the other table's invalid index must survive")
	assert.False(t, valid, "the other table's index must keep its invalid state for its own recovery")
}

func TestBuildIndexConcurrentlyCallerCancellationReportsItsLeftover(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// A repeatable-read transaction holding a snapshot makes the build
	// block in its wait-for-old-snapshots phase — after the invalid catalog
	// entry exists, before it can become valid.
	blocker, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})

	buildCtx, cancelBuild := context.WithCancel(t.Context())
	defer cancelBuild()
	done := make(chan error, 1)
	go func() {
		_, err := executor.BuildIndexConcurrently(buildCtx, pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_cancelled ON %s.t (c)", schema), buildBudget)
		done <- err
	}()

	// Wait until the build has created its catalog entry and is blocked,
	// then cancel the caller: the moment the leftover verdict matters most.
	// The blocker stays held for the whole verdict — the read-only catalog
	// inspection must not need the build's lock ever being granted.
	require.Eventually(t, func() bool {
		exists, _ := indexState(t, pool, schema, "idx_cancelled")
		return exists
	}, 30*time.Second, 50*time.Millisecond, "the blocked build must have created its catalog entry")
	cancelBuild()

	// The verdict runs under a detached bounded context: it must first
	// prove the server-side build aborted, then report the invalid entry —
	// never drop it.
	select {
	case err := <-done:
		require.Error(t, err, "the cancelled build must fail")
		require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
		var invalidErr *executor.InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.Equal(t, schema, invalidErr.Schema)
		assert.Equal(t, "idx_cancelled", invalidErr.Index)
		assert.ErrorIs(t, invalidErr.Build, context.Canceled, "the original cancellation must ride inside the typed outcome")
	case <-time.After(time.Minute):
		t.Fatal("the cancelled build did not return")
	}
	exists, valid := indexState(t, pool, schema, "idx_cancelled")
	assert.True(t, exists, "the invalid entry must survive for the operator's explicit recovery")
	assert.False(t, valid, "the leftover must be invalid")
}

func TestBuildIndexConcurrentlyCancellationBeforeCatalogEntryIsClean(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// Hold ACCESS EXCLUSIVE so the build blocks on its table lock — before
	// it can create any catalog entry. Cancelling here must not let
	// recovery race the still-waiting backend into a false clean verdict.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	buildCtx, cancelBuild := context.WithCancel(t.Context())
	defer cancelBuild()
	done := make(chan error, 1)
	go func() {
		_, err := executor.BuildIndexConcurrently(buildCtx, pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_early ON %s.t (c)", schema), buildBudget)
		done <- err
	}()

	// Wait until the build is provably waiting on the table lock, then
	// cancel. The blocker stays held for the whole recovery: proving the
	// backend stopped must not depend on the lock ever being granted.
	require.Eventually(t, func() bool {
		var waiting int
		err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE state = 'active' AND wait_event_type = 'Lock'
			    AND query LIKE 'CREATE INDEX CONCURRENTLY idx_early%'`).Scan(&waiting)
		require.NoError(t, err)
		return waiting == 1
	}, 30*time.Second, 50*time.Millisecond, "the build must be blocked on its table lock")
	cancelBuild()

	select {
	case err := <-done:
		require.Error(t, err, "the cancelled build must fail")
		var invalidErr *executor.InvalidIndexError
		require.NotErrorAs(t, err, &invalidErr,
			"a build cancelled before its catalog entry must resolve clean, with the backend stop proven")
	case <-time.After(time.Minute):
		t.Fatal("the cancelled build did not return")
	}
	exists, _ := indexState(t, pool, schema, "idx_early")
	assert.False(t, exists, "no catalog entry may exist after a pre-entry cancellation")
}

func TestBuildIndexConcurrentlyNeverDropsValidIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); CREATE INDEX idx_c ON %s.t (c)", schema, schema))
	require.NoError(t, err)

	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (id)", schema), buildBudget)

	// A genuine phase-1 collision over a valid index creates nothing, so
	// the verdict must pass PostgreSQL's error through unchanged — not
	// wrap it into a recovery outcome there is nothing to recover from.
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, sqlstateDuplicateTable, pgErr.Code, "a duplicate name over a valid index is PostgreSQL's error to make")
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr, "a provably clean catalog must not report a recovery outcome")
	exists, valid := indexState(t, pool, schema, "idx_c")
	assert.True(t, exists, "the valid index must survive")
	assert.True(t, valid, "the valid index must stay valid")
}

func TestBuildIndexConcurrentlyIfNotExistsIsRefused(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); CREATE INDEX idx_c ON %s.t (c)", schema, schema))
	require.NoError(t, err)

	// IF NOT EXISTS no-ops on the name alone: it would report success over
	// an index the executor cannot prove valid, related, or intended. The
	// refusal is an admission decision — nothing reaches the database.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_c ON %s.t (c)", schema), buildBudget)
	require.ErrorIs(t, err, executor.ErrIfNotExistsUnsupported)
	exists, valid := indexState(t, pool, schema, "idx_c")
	assert.True(t, exists, "the existing index must survive the refusal")
	assert.True(t, valid, "the existing index must stay valid")
}

func TestBuildIndexConcurrentlyOverallBudgetCancelsBlockedBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// Hold ACCESS EXCLUSIVE in an open transaction so the build blocks on
	// its table lock and the overall deadline — not a per-lock timeout —
	// cancels it.
	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	tight := executor.ConcurrentBudget{Overall: 300 * time.Millisecond}
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_blocked ON %s.t (c)", schema), tight)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseStatement, budgetErr.Cause, "the overall deadline is the statement budget")
	assert.Equal(t, tight.Overall, budgetErr.Budget)
}

func TestBuildIndexConcurrentlyBudgetCancellationLeavesNoDebris(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	blocker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = blocker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	tight := executor.ConcurrentBudget{Overall: 300 * time.Millisecond}
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_blocked ON %s.t (c)", schema), tight)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)

	// A build cancelled while waiting on its table lock never created a
	// catalog entry, so the budget error must arrive alone — no
	// *InvalidIndexError, nothing for an operator to recover.
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr, "a pre-entry cancellation must resolve clean")
	require.NoError(t, blocker.Rollback(t.Context()))
	exists, _ := indexState(t, pool, schema, "idx_blocked")
	assert.False(t, exists, "a cancelled build must leave no index entry behind")

	// The table must be immediately buildable again.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_blocked ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)
}

func TestBuildIndexConcurrentlyExpressionRaisedCollisionStillGetsVerdict(t *testing.T) {
	pool, schema := newPool(t)
	// An index-expression function that raises the name-collision SQLSTATE
	// (42P07) mid-build: the failure fires after the catalog entry exists,
	// so treating that SQLSTATE as "nothing was created" would skip the
	// verdict and silently strand the invalid leftover.
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (id int PRIMARY KEY, c int);
		INSERT INTO %s.t VALUES (1, 7);
		CREATE FUNCTION %s.boom(int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS
		$$ BEGIN RAISE USING ERRCODE = '42P07'; END $$`, schema, schema, schema))
	require.NoError(t, err)

	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_boom ON %s.t (%s.boom(c))", schema, schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, invalidErr.Build, &pgErr)
	assert.Equal(t, sqlstateDuplicateTable, pgErr.Code, "the expression's SQLSTATE must ride inside the typed outcome")
	exists, valid := indexState(t, pool, schema, "idx_boom")
	assert.True(t, exists, "the leftover must survive for the operator's explicit recovery")
	assert.False(t, valid, "the leftover must be invalid")
}

func TestBuildIndexConcurrentlyProofsResistCatalogShadowing(t *testing.T) {
	url := testutil.StartPostgres(t)
	bootstrap, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(bootstrap.Close)
	schema := testutil.NewSchema(t, bootstrap)

	// A user schema listed before pg_catalog on search_path shadows
	// unqualified catalog names. These impostors are empty or NULL:
	// consulted, they would resolve no table, report no invalid index, and
	// show no backend — turning every fail-closed proof into a false
	// clean. The executor's proof queries must see the real catalogs
	// through them.
	_, err = bootstrap.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.pg_class (oid oid, relname name, relnamespace oid);
		CREATE TABLE %[1]s.pg_namespace (oid oid, nspname name);
		CREATE TABLE %[1]s.pg_index (indexrelid oid, indisvalid boolean);
		CREATE TABLE %[1]s.pg_attribute (attrelid oid, attnum smallint, attnotnull boolean);
		CREATE TABLE %[1]s.pg_stat_activity (pid int, state text);
		CREATE FUNCTION %[1]s.to_regclass(text) RETURNS regclass
			LANGUAGE sql IMMUTABLE AS 'SELECT NULL::regclass';
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, c int);
		INSERT INTO %[1]s.t VALUES (1, 7), (2, 7)`, schema))
	require.NoError(t, err)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL: url,
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.RuntimeParams["search_path"] = schema + ", pg_catalog"
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Target resolution: the impostor to_regclass resolves everything to
	// NULL, so a shadowed resolution could never admit this build.
	rep, err := executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_ok ON %s.t (id)", schema), buildBudget)
	require.NoError(t, err, "target resolution must see the real catalog through the impostors")
	assert.Equal(t, schema, rep.Schema)
	assert.Equal(t, "idx_ok", rep.Index)
	assert.NotZero(t, rep.IndexOID)

	// The failure verdict: the impostor pg_class would resolve no table
	// and yield an identity verdict; the impostor pg_stat_activity would
	// hide the build backend. The real catalogs must name the leftover.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_dup ON %s.t (c)", schema), buildBudget)
	require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex,
		"the failure verdict must see the real catalog through the impostors")
	exists, valid := indexState(t, bootstrap, schema, "idx_dup")
	require.True(t, exists, "the failed build must leave its catalog entry")
	require.False(t, valid, "the leftover must be invalid")

	// The pre-build refusal: the impostor pg_index is empty and would miss
	// the debris, letting a new build run over unprovable state.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_dup ON %s.t (c)", schema), buildBudget)
	require.ErrorIs(t, err, executor.ErrPreexistingInvalidIndex,
		"the pre-build inspection must see the real catalog through the impostors")

	// The USING INDEX attachment proof: the impostor pg_index and
	// to_regclass would find no backing index and refuse a safe
	// attachment; the impostor pg_attribute would hide every NOT NULL. The
	// proof must see the real catalogs and admit this attachment.
	err = executor.ExecuteNativeSequence(t.Context(), pool, []string{
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY t_id_key ON %s.t (id)", schema),
		fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT t_id_key UNIQUE USING INDEX t_id_key", schema),
	}, budget, buildBudget)
	require.NoError(t, err, "the attachment proof must see the real catalog through the impostors")
	var attached bool
	require.NoError(t, bootstrap.QueryRow(t.Context(),
		`SELECT COALESCE(bool_or(contype = 'u'), false) FROM pg_constraint WHERE conrelid = $1::regclass`,
		fmt.Sprintf("%s.t", schema)).Scan(&attached))
	assert.True(t, attached, "the UNIQUE constraint must be attached")
}

func TestBuildIndexConcurrentlyTerminatedBackendReportsItsLeftover(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	// A repeatable-read snapshot blocks the build in its wait phase, after
	// its catalog entry exists; terminating the backend then models the
	// harshest failure — the build session dies mid-statement.
	blocker, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	t.Cleanup(func() {
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})

	done := make(chan error, 1)
	go func() {
		_, err := executor.BuildIndexConcurrently(t.Context(), pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_term ON %s.t (c)", schema), buildBudget)
		done <- err
	}()

	require.Eventually(t, func() bool {
		exists, _ := indexState(t, pool, schema, "idx_term")
		return exists
	}, 30*time.Second, 50*time.Millisecond, "the blocked build must have created its catalog entry")
	var terminated bool
	err = pool.QueryRow(t.Context(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		  WHERE query LIKE 'CREATE INDEX CONCURRENTLY idx_term%'`).Scan(&terminated)
	require.NoError(t, err)
	require.True(t, terminated, "the build backend must have been terminated")

	select {
	case err := <-done:
		require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
		var invalidErr *executor.InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.Equal(t, schema, invalidErr.Schema)
		assert.Equal(t, "idx_term", invalidErr.Index)
	case <-time.After(time.Minute):
		t.Fatal("the terminated build did not return")
	}
	exists, valid := indexState(t, pool, schema, "idx_term")
	assert.True(t, exists, "the leftover must survive for the operator's explicit recovery")
	assert.False(t, valid, "the leftover must be invalid")

	// The dead session's RESET cannot succeed, so it must have been
	// discarded — not released: no session in the pool may still carry the
	// build's lock_timeout = 0 override.
	for _, conn := range pool.AcquireAllIdle(t.Context()) {
		var lockTimeout string
		require.NoError(t, conn.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&lockTimeout))
		assert.NotEqual(t, "0", lockTimeout, "no pooled session may keep the build's disabled lock_timeout")
		conn.Release()
	}
}
