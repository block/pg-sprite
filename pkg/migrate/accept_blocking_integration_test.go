package migrate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// acceptBlockingOptions is the runnable policy with the acknowledgement
// set and the brief budgets tightened so a contended or slow statement
// resolves in well under a second.
func acceptBlockingOptions(table string) migrate.Options {
	opts := runOptions()
	opts.AcceptBlocking = table
	opts.Budget.Brief = executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
	return opts
}

// indexExists reports whether the named index is present in the schema.
func indexExists(t *testing.T, pool *pgxpool.Pool, schema, index string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = $2)`,
		schema, index).Scan(&exists))
	return exists
}

// The acknowledgement turns exactly the eligible gate refusals into a
// bounded blocking execution: the statement runs as submitted, the verdict
// carries the refusal's identity plus the budgets it ran under, and the
// acknowledged table is the one the catalog says the statement locks.
func TestRunAcceptBlockingExecutesEligibleRefusals(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (
				id int PRIMARY KEY,
				created_at timestamptz
			);
			CREATE INDEX orders_created_idx ON %[1]s.orders (created_at)`, schema))
		require.NoError(t, err)
		return schema
	}

	assertAccepted := func(t *testing.T, v verdict.Verdict, schema string) {
		t.Helper()
		assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
		assert.True(t, v.BlockingPassthrough)
		assert.Equal(t, schema+".orders", v.Table)
		assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
		assert.Equal(t, verdict.ClassByDesign, v.Class)
		assert.Equal(t, "100ms", v.LockTimeout)
		assert.Equal(t, "5s", v.StatementTimeout)
		assert.False(t, v.Forced, "accepting a blocking form is not the force acknowledgement")
	}

	t.Run("DROP INDEX drops the index under budgets", func(t *testing.T) {
		schema := seed(t)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_created_idx", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.NoError(t, err)
		assertAccepted(t, v, schema)
		assert.False(t, indexExists(t, pool, schema, "orders_created_idx"), "the accepted DROP INDEX must have committed")
	})

	t.Run("REINDEX INDEX resolves the owning table from the catalog", func(t *testing.T) {
		schema := seed(t)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("REINDEX INDEX %s.orders_created_idx", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.NoError(t, err)
		assertAccepted(t, v, schema)
		assert.True(t, indexExists(t, pool, schema, "orders_created_idx"), "REINDEX keeps the index")
	})

	t.Run("REINDEX TABLE acknowledges the table itself", func(t *testing.T) {
		schema := seed(t)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("REINDEX TABLE %s.orders", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.NoError(t, err)
		assertAccepted(t, v, schema)
	})

	t.Run("an unqualified index resolves on the session search_path", func(t *testing.T) {
		schema := seed(t)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX shared_idx ON %s.orders (id)", schema))
		require.NoError(t, err)

		// The statement and the catalog lookup share the pool, so the
		// acknowledgement must name the schema the pool resolves the bare
		// name in; this pool's search_path is the default, which does not
		// contain the test schema, so the relation is not found and nothing
		// runs.
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, "DROP INDEX shared_idx"),
			acceptBlockingOptions(schema+".orders"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingRelationNotFound)
		assert.Equal(t, verdict.Verdict{}, v)
		assert.True(t, indexExists(t, pool, schema, "shared_idx"))
	})
}

// The acknowledgement is a claim about the table the statement locks. When
// the claim is false — it names another table, or the relation does not
// exist — nothing executes: no verdict, a typed error, and the index still
// present.
func TestRunAcceptBlockingRejectsFalseAcknowledgements(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
		CREATE TABLE %[1]s.customers (id int PRIMARY KEY);
		CREATE INDEX orders_id_idx ON %[1]s.orders (id)`, schema))
	require.NoError(t, err)

	t.Run("naming a table the statement does not lock", func(t *testing.T) {
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_id_idx", schema)),
			acceptBlockingOptions(schema+".customers"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
		assert.Equal(t, verdict.Verdict{}, v)
		assert.True(t, indexExists(t, pool, schema, "orders_id_idx"), "a mismatched acknowledgement must execute nothing")
	})

	t.Run("naming a table for an index that does not exist", func(t *testing.T) {
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.no_such_idx", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingRelationNotFound)
		assert.Equal(t, verdict.Verdict{}, v)
	})

	t.Run("naming a table for a REINDEX TABLE that does not exist", func(t *testing.T) {
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("REINDEX TABLE %s.no_such_table", schema)),
			acceptBlockingOptions(schema+".no_such_table"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingRelationNotFound)
		assert.Equal(t, verdict.Verdict{}, v)
	})

	// The relation exists, so "not visible on the search_path" would send
	// the operator to check grants for a statement that simply names the
	// wrong kind of relation. The error names the kind found instead.
	t.Run("DROP INDEX naming a table is the wrong kind, not a missing relation", func(t *testing.T) {
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.orders", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingWrongRelationKind)
		assert.NotErrorIs(t, err, migrate.ErrAcceptBlockingRelationNotFound)
		assert.Equal(t, verdict.Verdict{}, v)
		assert.True(t, indexExists(t, pool, schema, "orders_id_idx"))
	})

	t.Run("REINDEX TABLE naming an index is the wrong kind, not a missing relation", func(t *testing.T) {
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("REINDEX TABLE %s.orders_id_idx", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.ErrorIs(t, err, migrate.ErrAcceptBlockingWrongRelationKind)
		assert.NotErrorIs(t, err, migrate.ErrAcceptBlockingRelationNotFound)
		assert.Equal(t, verdict.Verdict{}, v)
	})
}

// A caller whose context ends while the accepted statement is waiting on
// its lock has lost the transaction from the client side: the statement
// was submitted, and whether it committed cannot be established from
// here. The failed verdict says so instead of claiming the rollback an
// ordinary failed attempt gets, and its code sends the operator to the
// catalog before any retry.
func TestRunAcceptBlockingReportsAnUnknownOutcomeHonestly(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
		CREATE INDEX orders_id_idx ON %[1]s.orders (id)`, schema))
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.orders IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	// The lock budget is long enough that only the caller's cancellation
	// can end the wait, so the outcome the test observes is the
	// cancellation's, not the budget's.
	opts := acceptBlockingOptions(schema + ".orders")
	opts.Budget.Brief.LockTimeout = 30 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		v   verdict.Verdict
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := migrate.Run(ctx, pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_id_idx", schema)), opts)
		done <- result{v, err}
	}()

	// Cancel only once the DROP INDEX is provably submitted and waiting on
	// the held lock; the polling query's own text does not start with it.
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			  WHERE query LIKE 'DROP INDEX %' AND wait_event_type = 'Lock')`).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 25*time.Millisecond, "the accepted DROP INDEX must be waiting on the holder's lock")
	cancel()

	select {
	case res := <-done:
		var unknownErr *executor.BlockingOutcomeUnknownError
		require.ErrorAs(t, res.err, &unknownErr)
		assert.Equal(t, verdict.OutcomeFailed, res.v.Outcome)
		assert.Equal(t, string(executor.CodeBlockingOutcomeUnknown), res.v.Code)
		assert.Equal(t, schema+".orders", res.v.Table)
		assert.False(t, res.v.BlockingPassthrough)
		assert.NotContains(t, res.v.Detail, "nothing committed",
			"the client cannot establish that fact, so the verdict must not claim it")
	case <-time.After(30 * time.Second):
		t.Fatal("the cancelled accepted execution must return")
	}
	require.NoError(t, holder.Rollback(t.Context()))
	assert.True(t, indexExists(t, pool, schema, "orders_id_idx"),
		"the lock was never granted, so the server-side outcome here is a rollback — which the client still could not know")
}

// The budgets bound the accepted statement in both directions. An
// ungranted lock is a refusal — the statement never ran, so the refusal
// contract applies and the index stands. A statement cut off by its
// budget after the lock was granted is a failure: the work the operator
// accepted was under way, and the failed verdict names the outcome code.
func TestRunAcceptBlockingBoundsTheStatement(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	t.Run("an ungranted lock is a refusal and nothing executes", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
			CREATE INDEX orders_id_idx ON %[1]s.orders (id)`, schema))
		require.NoError(t, err)
		holder, err := pool.Begin(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
		_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.orders IN ACCESS EXCLUSIVE MODE", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.orders_id_idx", schema)),
			acceptBlockingOptions(schema+".orders"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
		assert.Equal(t, verdict.CauseLockBudget, v.Cause)
		assert.Equal(t, schema+".orders", v.Table)
		assert.False(t, v.BlockingPassthrough, "a refusal is not a passthrough execution")
		require.NoError(t, holder.Rollback(t.Context()))
		assert.True(t, indexExists(t, pool, schema, "orders_id_idx"), "the lock was never granted, so the index stands")
	})

	t.Run("a statement cut off by its budget is a failure", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		// An index over a function that sleeps per row makes the rebuild
		// take longer than the statement budget: ten rows at 50ms each is
		// half a second against a 100ms bound, so the cancellation is
		// deterministic and the whole test still finishes quickly.
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.orders (id int PRIMARY KEY);
			INSERT INTO %[1]s.orders SELECT g FROM generate_series(1, 10) g;
			CREATE FUNCTION %[1]s.slow_key(int) RETURNS int LANGUAGE plpgsql IMMUTABLE AS $$
			BEGIN
				PERFORM pg_sleep(0.05);
				RETURN $1;
			END $$;
			CREATE INDEX orders_slow_idx ON %[1]s.orders (%[1]s.slow_key(id))`, schema))
		require.NoError(t, err)
		opts := acceptBlockingOptions(schema + ".orders")
		opts.Budget.Brief.StatementTimeout = 100 * time.Millisecond

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("REINDEX INDEX %s.orders_slow_idx", schema)), opts)
		var budgetErr *executor.BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, executor.CauseStatement, budgetErr.Cause)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeBudgetStatementExceeded), v.Code)
		assert.Equal(t, schema+".orders", v.Table)
		assert.False(t, v.BlockingPassthrough)
		assert.True(t, indexExists(t, pool, schema, "orders_slow_idx"), "a cancelled REINDEX rolls back and leaves the index")
	})
}

// REINDEX of a partitioned relation is eligible by shape but PostgreSQL
// will not run it inside a transaction block, which is the only way the
// accepted path runs anything. That is a permanent capability boundary,
// reported as an unsupported-statement refusal rather than a coded failure,
// and the parent keeps its indexes.
func TestRunAcceptBlockingRefusesReindexOnPartitionedParent(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id int,
			day date
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE INDEX events_day_idx ON %[1]s.events (day)`, schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("REINDEX TABLE %s.events", schema)),
		acceptBlockingOptions(schema+".events"))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, schema+".events", v.Table)
	assert.False(t, v.BlockingPassthrough)
	assert.True(t, indexExists(t, pool, schema, "events_day_idx"))
}
