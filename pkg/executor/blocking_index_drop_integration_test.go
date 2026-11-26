package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// seedPartitionedKindIndex creates a range-partitioned events table with
// two partitions and an index on the parent, which PostgreSQL makes a
// partitioned index with one attached index per partition, and a plain
// table with an index of its own. It returns the definition the plan reads
// for the parent's index.
func seedPartitionedKindIndex(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			kind text NOT NULL,
			created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

		CREATE INDEX events_kind_idx ON %[1]s.events (kind);

		CREATE TABLE %[1]s.users (
			id bigint PRIMARY KEY,
			email text
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email);`, schema))
	require.NoError(t, err)
	live, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "events", schemadiff.DroppedIndex, "events_kind_idx")
	require.NoError(t, err)
	require.True(t, found)
	return live.Definition
}

// schemaIndexes lists the indexes in the schema.
func schemaIndexes(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT indexname FROM pg_indexes WHERE schemaname = $1 ORDER BY indexname`, schema)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

var blockingDropBudget = executor.BlockingBudget{LockTimeout: time.Second, StatementTimeout: 10 * time.Second}

// The plain DROP INDEX of a partitioned table's index removes the parent's
// index and every partition's index in one transaction, and reports the
// partition indexes it removed.
func TestExecuteBlockingPartitionedIndexDropDropsTheTree(t *testing.T) {
	pool, schema := newPool(t)
	definition := seedPartitionedKindIndex(t, pool, schema)

	rep, err := executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, "events", "events_kind_idx", definition, blockingDropBudget)
	require.NoError(t, err)
	assert.Equal(t, []string{
		schema + ".events_2026_01_kind_idx",
		schema + ".events_2026_02_kind_idx",
	}, rep.PartitionIndexes)
	assert.NotZero(t, rep.IndexOID)
	assert.Equal(t, []string{"users_email_idx", "users_pkey"}, schemaIndexes(t, pool, schema),
		"only the plain table's indexes remain")
}

// The index is read again under the table's lock and the drop refused, with
// nothing dropped, unless it is still the parent's partitioned index with
// the definition the plan reviewed.
func TestExecuteBlockingPartitionedIndexDropRefusesDrift(t *testing.T) {
	for _, tc := range []struct {
		name, table, index string
		definition         func(reviewed string) string
	}{
		{
			name: "a definition other than the reviewed one", table: "events", index: "events_kind_idx",
			definition: func(string) string {
				return "CREATE INDEX events_kind_idx ON ONLY app.events USING btree (kind, id)"
			},
		},
		{
			// An attached partition index never appears in a desired plan;
			// the read refuses it anyway, since PostgreSQL drops it only
			// with its parent.
			name: "a partition's attached index", table: "events_2026_01", index: "events_2026_01_kind_idx",
			definition: func(reviewed string) string { return reviewed },
		},
		{
			name: "a plain table's index", table: "users", index: "users_email_idx",
			definition: func(reviewed string) string { return reviewed },
		},
		{
			name: "an index that is gone", table: "events", index: "events_missing_idx",
			definition: func(reviewed string) string { return reviewed },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := newPool(t)
			reviewed := seedPartitionedKindIndex(t, pool, schema)
			before := schemaIndexes(t, pool, schema)

			_, err := executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, tc.table, tc.index,
				tc.definition(reviewed), blockingDropBudget)
			require.ErrorIs(t, err, executor.ErrDroppedIndexDrift)
			assert.Equal(t, before, schemaIndexes(t, pool, schema), "nothing was dropped")
		})
	}
}

// A session holding ACCESS SHARE on one partition keeps the parent's lock
// from being granted: the request ends at the lock budget, as a lock
// budget error, and nothing is dropped.
func TestExecuteBlockingPartitionedIndexDropLockBudgetDropsNothing(t *testing.T) {
	pool, schema := newPool(t)
	definition := seedPartitionedKindIndex(t, pool, schema)
	before := schemaIndexes(t, pool, schema)
	reader, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.events_2026_02 IN ACCESS SHARE MODE`, schema))
	require.NoError(t, err)

	b := executor.BlockingBudget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 10 * time.Second}
	_, err = executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, "events", "events_kind_idx", definition, b)
	require.NoError(t, reader.Rollback(t.Context()))
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Equal(t, b.LockTimeout, budgetErr.Budget)
	assert.Equal(t, before, schemaIndexes(t, pool, schema), "nothing was dropped")
}

// The stated lock budget, not the session's own lock_timeout, ends the
// wait: on a pool whose sessions wait 30s for a lock, a 200ms budget is
// refused long before the session default would fire, with nothing dropped.
func TestExecuteBlockingPartitionedIndexDropWaitsOnlyTheStatedLockBudget(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:              testutil.StartPostgres(t),
		LockTimeout:      30 * time.Second,
		StatementTimeout: time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	definition := seedPartitionedKindIndex(t, pool, schema)
	before := schemaIndexes(t, pool, schema)
	reader, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.events_2026_02 IN ACCESS SHARE MODE`, schema))
	require.NoError(t, err)

	b := executor.BlockingBudget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 50 * time.Second}
	start := time.Now()
	_, err = executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, "events", "events_kind_idx", definition, b)
	elapsed := time.Since(start)
	require.NoError(t, reader.Rollback(t.Context()))
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Less(t, elapsed, 10*time.Second, "the 200ms budget, not the session's 30s lock_timeout, ended the wait")
	assert.Equal(t, before, schemaIndexes(t, pool, schema), "nothing was dropped")
}

// The index is read only once the parent's lock is granted: a session that
// holds the lock, swaps the index for another definition under the same
// name, and commits while the drop waits leaves the drop to read the swapped
// index and refuse it as drift, so the index the plan never reviewed stands.
func TestExecuteBlockingPartitionedIndexDropReadsTheIndexUnderTheParentLock(t *testing.T) {
	pool, schema := newPool(t)
	definition := seedPartitionedKindIndex(t, pool, schema)
	swapper, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := swapper.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back the swapper: %v", err)
		}
	})
	_, err = swapper.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.events IN ACCESS EXCLUSIVE MODE`, schema))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, "events", "events_kind_idx", definition,
			executor.BlockingBudget{LockTimeout: 10 * time.Second, StatementTimeout: 20 * time.Second})
		done <- err
	}()

	const lockWaitDeadline = 5 * time.Second
	require.Eventually(t, func() bool {
		var waiting int
		err := pool.QueryRow(t.Context(), `
			SELECT count(*)
			  FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock'
			   AND position($1 in query) > 0`, schema).Scan(&waiting)
		return err == nil && waiting == 1
	}, lockWaitDeadline, 50*time.Millisecond, "the drop waits on the swapper's lock")

	_, err = swapper.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.events_kind_idx;
		CREATE INDEX events_kind_idx ON %[1]s.events (kind, id);`, schema))
	require.NoError(t, err)
	require.NoError(t, swapper.Commit(t.Context()))

	require.ErrorIs(t, <-done, executor.ErrDroppedIndexDrift)
	assert.Contains(t, schemaIndexes(t, pool, schema), "events_kind_idx", "the swapped-in index stands")
}

// A drop that would remove a partition's replica identity index is refused,
// with nothing dropped: the partition's UPDATE and DELETE would fail while a
// publication publishes them.
func TestExecuteBlockingPartitionedIndexDropRefusesAPartitionsReplicaIdentityIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');

		CREATE UNIQUE INDEX events_uq ON %[1]s.events (id, created_at);

		ALTER TABLE %[1]s.events_2026 REPLICA IDENTITY USING INDEX events_2026_id_created_at_idx;`, schema))
	require.NoError(t, err)
	live, found, err := schemadiff.ReadDroppedObject(t.Context(), pool, schema, "events", schemadiff.DroppedIndex, "events_uq")
	require.NoError(t, err)
	require.True(t, found)
	before := schemaIndexes(t, pool, schema)

	_, err = executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), pool, schema, "events", "events_uq", live.Definition, blockingDropBudget)
	require.ErrorIs(t, err, executor.ErrUnsupportedAcceptedBlocking)
	assert.Equal(t, before, schemaIndexes(t, pool, schema), "nothing was dropped")
}

// A budget whose statement bound is not longer than its lock bound is
// refused before a session is used.
func TestExecuteBlockingPartitionedIndexDropRefusesAnInvalidBudget(t *testing.T) {
	_, err := executor.ExecuteBlockingPartitionedIndexDrop(t.Context(), nil, "app", "events", "events_kind_idx", "CREATE INDEX ...",
		executor.BlockingBudget{LockTimeout: time.Second, StatementTimeout: time.Second})
	require.ErrorIs(t, err, executor.ErrInvalidBlockingBudget)
}
