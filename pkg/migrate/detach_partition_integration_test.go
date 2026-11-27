package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// detachPool returns a pool on a database of its own. DETACH PARTITION ...
// FINALIZE waits for every older transaction in its database, so on a
// server other packages share, their transactions would hold it past the
// tests' lock budgets.
func detachPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, testutil.StartPostgres(t))})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedEvents creates events, range-partitioned by day, with a 2024 and a
// 2025 partition holding 100 rows each. A test that needs a DEFAULT
// partition adds it.
func seedEvents(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			day date NOT NULL,
			PRIMARY KEY (id, day)
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
		CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
		INSERT INTO %[1]s.events SELECT g, date '2024-06-01' FROM generate_series(1, 100) g;
		INSERT INTO %[1]s.events SELECT g, date '2025-06-01' FROM generate_series(1, 100) g`, schema))
	require.NoError(t, err)
	return schema
}

// detachOptions are the run options with the destructive acknowledgement
// for schema.events, which every detach needs.
func detachOptions(schema string) migrate.Options {
	opts := runOptions()
	opts.AllowDestructive = schema + ".events"
	return opts
}

// partitionState reports whether schema.table is a partition of
// schema.events, and whether its detach is pending.
func partitionState(t *testing.T, pool *pgxpool.Pool, schema, table string) (attached, pending bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `
		SELECT true, inhdetachpending
		  FROM pg_inherits
		 WHERE inhrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
		   AND inhparent = (quote_ident($1) || '.events')::regclass`,
		schema, table).Scan(&attached, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return attached, pending
}

// openReader begins a transaction that reads events and stays open until
// the returned function ends it. A concurrent detach waits for it after its
// first transaction commits. A test that stops before ending the reader
// ends it in cleanup, so the pool's close does not wait on its connection.
func openReader(t *testing.T, pool *pgxpool.Pool, schema string) func() {
	t.Helper()
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	var once sync.Once
	end := func() {
		once.Do(func() {
			defer conn.Release()
			_, err := conn.Exec(context.WithoutCancel(t.Context()), "COMMIT")
			assert.NoError(t, err, "end the reader")
		})
	}
	t.Cleanup(end)
	_, err = conn.Exec(t.Context(), "BEGIN")
	require.NoError(t, err)
	var n int
	require.NoError(t, conn.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.events", schema)).Scan(&n))
	require.Equal(t, 200, n)
	return end
}

// waitForDetachToWait waits until a DETACH PARTITION statement on a table
// in schema is waiting on a lock: for a concurrent detach after its first
// transaction, the wait for the reader's transaction.
func waitForDetachToWait(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	const detachWaitDeadline = 30 * time.Second
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			 WHERE query LIKE 'ALTER TABLE % DETACH PARTITION %'
			   AND strpos(query, $1) > 0
			   AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid())`, schema).Scan(&waiting)
		return err == nil && waiting
	}, detachWaitDeadline, 25*time.Millisecond, "the detach must be waiting for the reader")
}

// A plain DETACH PARTITION runs as DETACH PARTITION ... CONCURRENTLY.
// While it waits for a transaction that still reads the table, writes to
// the table's other partition keep committing under a short lock_timeout:
// the detach holds SHARE UPDATE EXCLUSIVE on the table, not the ACCESS
// EXCLUSIVE the plain form takes. Once the reader ends, the detach finishes
// and the partition keeps its rows as a table of its own.
func TestRunDetachesAPartitionConcurrentlyWhileWritesCommit(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	endReader := openReader(t, pool, schema)

	type result struct {
		v   verdict.Verdict
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
			"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024", schema)), detachOptions(schema))
		done <- result{v, err}
	}()
	waitForDetachToWait(t, pool, schema)

	for i := 1; i <= 20; i++ {
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "SET LOCAL lock_timeout = '500ms'")
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), fmt.Sprintf(
			"INSERT INTO %s.events VALUES ($1, date '2025-07-01')", schema), 1000+i)
		require.NoError(t, err, "a write to another partition commits while the detach waits")
		require.NoError(t, tx.Commit(t.Context()))
	}
	select {
	case res := <-done:
		t.Fatalf("the detach finished while the reader was still open: %+v, %v", res.v, res.err)
	default:
	}
	endReader()

	res := <-done
	require.NoError(t, res.err)
	assert.Equal(t, verdict.OutcomeExecuted, res.v.Outcome)
	assert.True(t, res.v.AllowedDestructive)
	assert.Equal(t, []string{fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)}, res.v.ExecutedSQL)
	attached, _ := partitionState(t, pool, schema, "events_2024")
	assert.False(t, attached)
	var inTable, inPartition int
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(
		"SELECT (SELECT count(*) FROM %[1]s.events), (SELECT count(*) FROM %[1]s.events_2024)", schema)).
		Scan(&inTable, &inPartition))
	assert.Equal(t, 120, inTable, "the 2025 partition's 100 rows and the 20 written during the detach")
	assert.Equal(t, 100, inPartition, "the detached partition keeps its rows")
}

// A concurrent detach that runs out of its lock budget after its first
// transaction committed leaves the partition detach-pending, and the
// verdict says so with its own code instead of claiming nothing committed.
// Running the same statement again finds the pending partition and
// finishes it with FINALIZE.
func TestRunFinishesADetachItsBudgetInterrupted(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	sql := fmt.Sprintf("ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)
	opts := detachOptions(schema)
	opts.Budget.Brief.LockTimeout = 300 * time.Millisecond
	endReader := openReader(t, pool, schema)

	// The reader outlasts every attempt: the first waits for it after
	// committing its first transaction, and the retries' FINALIZE waits for
	// the lock on the partition the reader holds.
	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.ErrorIs(t, err, executor.ErrDetachInterrupted)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeDetachInterrupted), v.Code)
	assert.Contains(t, v.Detail, fmt.Sprintf("ALTER TABLE %s DETACH PARTITION %s FINALIZE",
		pgx.Identifier{schema, "events"}.Sanitize(), pgx.Identifier{schema, "events_2024"}.Sanitize()))
	attached, pending := partitionState(t, pool, schema, "events_2024")
	assert.True(t, attached)
	assert.True(t, pending, "the first transaction committed, so the partition is detach-pending")
	endReader()

	v, err = migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Contains(t, v.Detail, "left detach-pending by an earlier run")
	attached, _ = partitionState(t, pool, schema, "events_2024")
	assert.False(t, attached, "the rerun finished the detach")
}

// PostgreSQL refuses a concurrent detach from a table that has a DEFAULT
// partition, so both forms are refused before anything runs, by design,
// with the plain detach under the force acknowledgement named as the way
// through. That forced plain detach then runs as written.
func TestRunRefusesAConcurrentDetachBesideADefaultPartition(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT", schema))
	require.NoError(t, err)

	for _, sql := range []string{
		fmt.Sprintf("ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024", schema),
		fmt.Sprintf("ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema),
	} {
		v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), detachOptions(schema))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome, sql)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason, sql)
		assert.Equal(t, verdict.ClassByDesign, v.Class, sql)
		assert.Contains(t, v.Detail, schema+".events_default", sql)
		attached, pending := partitionState(t, pool, schema, "events_2024")
		assert.True(t, attached, "nothing ran: %s", sql)
		assert.False(t, pending, "nothing ran: %s", sql)
	}

	opts := detachOptions(schema)
	opts.Force = schema + ".events"
	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024", schema)), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.Forced)
	attached, _ := partitionState(t, pool, schema, "events_2024")
	assert.False(t, attached, "the forced plain detach ran")
}

// A detach takes the partition's rows out of the table, so it is refused
// without the destructive acknowledgement, like a drop, and nothing runs.
func TestRunRefusesADetachWithoutTheDestructiveAcknowledgement(t *testing.T) {
	pool := detachPool(t)
	schema := seedEvents(t, pool)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2024 CONCURRENTLY", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonDestructiveChange, v.Reason)
	attached, _ := partitionState(t, pool, schema, "events_2024")
	assert.True(t, attached)
}
