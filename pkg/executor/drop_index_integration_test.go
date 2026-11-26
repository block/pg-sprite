package executor_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// anyIndexDefinition stands in for the definition of an index the check
// refuses before it ever compares definitions.
const anyIndexDefinition = "CREATE INDEX any_idx ON t USING btree (a)"

var dropBudget = executor.ConcurrentBudget{Overall: time.Minute}

// reviewedDefinition is the definition the plan carries for the table's
// index, as the diff derives it.
func reviewedDefinition(t *testing.T, pool *pgxpool.Pool, schema, index string) string {
	t.Helper()
	return modeltest.PlannedDrop(t, pool, schema, "t", schemadiff.DroppedIndex, index).Definition
}

// holdOldTransaction opens a transaction that has read the table. A
// DROP INDEX CONCURRENTLY of the table's index waits for it to end.
func holdOldTransaction(t *testing.T, pool *pgxpool.Pool, schema string) pgx.Tx {
	t.Helper()
	old, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Rollback(context.WithoutCancel(t.Context())) })
	var n int
	require.NoError(t, old.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	return old
}

// A reviewed index drop runs as DROP INDEX CONCURRENTLY on the table's own
// plain, valid index and reports the OID it checked. The table keeps every
// other index.
func TestExecuteDropIndexConcurrentlyDropsAPlainIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int, b int);
		CREATE INDEX t_a_idx ON %[1]s.t (a);
		CREATE INDEX t_b_idx ON %[1]s.t (b)`, schema))
	require.NoError(t, err)
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT $1::regclass::oid`, schema+".t_a_idx").Scan(&oid))

	dropped, err := executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", reviewedDefinition(t, pool, schema, "t_a_idx"), dropBudget)
	require.NoError(t, err)
	assert.Equal(t, schema, dropped.Schema)
	assert.Equal(t, "t_a_idx", dropped.Index)
	assert.Equal(t, oid, dropped.IndexOID)
	assert.Positive(t, dropped.Duration)

	exists, _ := indexState(t, pool, schema, "t_a_idx")
	assert.False(t, exists, "the reviewed index is gone")
	exists, valid := indexState(t, pool, schema, "t_b_idx")
	assert.True(t, exists && valid, "an index the plan did not name survives")
}

// Each case is an index the plan's facts could have vouched for that is not
// a plain, valid index of the plan's table when the drop reads the catalog:
// nothing is dropped and the code is the permanent dropped-index-drift.
func TestExecuteDropIndexConcurrentlyRefusesAnIndexThatChanged(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int, handle text NOT NULL, CONSTRAINT t_handle_key UNIQUE (handle));
		CREATE TABLE %[1]s.other (id int PRIMARY KEY, note text);
		CREATE INDEX other_note_idx ON %[1]s.other (note);
		CREATE TABLE %[1]s.dup (id int PRIMARY KEY, v int);
		INSERT INTO %[1]s.dup VALUES (1, 7), (2, 7);
		CREATE TABLE %[1]s.parted (id int, v int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.parted_1 PARTITION OF %[1]s.parted FOR VALUES FROM (0) TO (100);
		CREATE INDEX parted_v_idx ON %[1]s.parted (v)`, schema))
	require.NoError(t, err)
	// A unique index build over duplicate rows fails and leaves an invalid
	// index behind.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY dup_v_idx ON %s.dup (v)", schema))
	require.Error(t, err)
	exists, valid := indexState(t, pool, schema, "dup_v_idx")
	require.True(t, exists)
	require.False(t, valid)

	cases := []struct {
		name  string
		table string
		index string
		want  string
	}{
		{"index that is gone", "t", "t_missing_idx", "has no index"},
		{"index of another table", "t", "other_note_idx", "has no index"},
		{"invalid index", "dup", "dup_v_idx", "invalid"},
		{"index behind a constraint", "t", "t_handle_key", "constraint's index"},
		{"primary key index", "t", "t_pkey", "constraint's index"},
		{"partitioned table's index", "parted", "parted_v_idx", "partitioned table's index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, tc.table, tc.index, anyIndexDefinition, dropBudget)
			require.ErrorIs(t, err, executor.ErrDroppedIndexDrift)
			assert.Equal(t, executor.CodeDroppedIndexDrift, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	for _, index := range []string{"other_note_idx", "t_handle_key", "t_pkey", "parted_v_idx", "dup_v_idx"} {
		exists, _ := indexState(t, pool, schema, index)
		assert.True(t, exists, "%s must survive a refused drop", index)
	}
	var partitionIndexes int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		  WHERE i.indrelid = $1::regclass AND c.relkind = 'i'`, schema+".parted_1").Scan(&partitionIndexes))
	assert.Equal(t, 1, partitionIndexes, "the partition's own index must survive")

	t.Run("index partition", func(t *testing.T) {
		var partitionIndex string
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			  WHERE i.indrelid = $1::regclass`, schema+".parted_1").Scan(&partitionIndex))
		_, err := executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "parted_1", partitionIndex, anyIndexDefinition, dropBudget)
		require.ErrorIs(t, err, executor.ErrDroppedIndexDrift)
		exists, _ := indexState(t, pool, schema, partitionIndex)
		assert.True(t, exists)
	})
}

// An index replaced under its name after planning is another index: the
// drop the plan reviewed refuses on the session that would drop it, and the
// replacement survives. Its own definition drops it.
func TestExecuteDropIndexConcurrentlyRefusesAReplacedIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int, b int);
		CREATE INDEX t_a_idx ON %[1]s.t (a)`, schema))
	require.NoError(t, err)
	reviewed := reviewedDefinition(t, pool, schema, "t_a_idx")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.t_a_idx;
		CREATE INDEX t_a_idx ON %[1]s.t (b)`, schema))
	require.NoError(t, err)

	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", reviewed, dropBudget)
	require.ErrorIs(t, err, executor.ErrDroppedIndexDrift)
	assert.Equal(t, executor.CodeDroppedIndexDrift, executor.OutcomeCode(err))
	exists, valid := indexState(t, pool, schema, "t_a_idx")
	assert.True(t, exists && valid, "the replacement index must survive a refused drop")

	current := reviewedDefinition(t, pool, schema, "t_a_idx")
	require.NotEqual(t, reviewed, current)
	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", current, dropBudget)
	require.NoError(t, err)
	exists, _ = indexState(t, pool, schema, "t_a_idx")
	assert.False(t, exists)
}

// The replica identity index of a table that publishes updates cannot be
// dropped without breaking UPDATE and DELETE, so the drop is refused and the
// index stays. After the identity moves, the same index drops.
func TestExecuteDropIndexConcurrentlyRefusesTheReplicaIdentityIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, k int NOT NULL);
		CREATE UNIQUE INDEX t_k_idx ON %[1]s.t (k);
		ALTER TABLE %[1]s.t REPLICA IDENTITY USING INDEX t_k_idx`, schema))
	require.NoError(t, err)

	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_k_idx", reviewedDefinition(t, pool, schema, "t_k_idx"), dropBudget)
	require.ErrorIs(t, err, executor.ErrReplicaIdentityDrop)
	assert.Equal(t, executor.CodeReplicaIdentityDrop, executor.OutcomeCode(err))
	exists, valid := indexState(t, pool, schema, "t_k_idx")
	assert.True(t, exists && valid)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t REPLICA IDENTITY DEFAULT", schema))
	require.NoError(t, err)
	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_k_idx", reviewedDefinition(t, pool, schema, "t_k_idx"), dropBudget)
	require.NoError(t, err)
	exists, _ = indexState(t, pool, schema, "t_k_idx")
	assert.False(t, exists)
}

// Unusable inputs, including a drop with no definition to check, fail before
// any session is used.
func TestExecuteDropIndexConcurrentlyRejectsBadInputs(t *testing.T) {
	pool, schema := newPool(t)

	_, err := executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "i", anyIndexDefinition, executor.ConcurrentBudget{})
	require.Error(t, err, "a budget with no bound is refused")

	uncancellable := context.WithoutCancel(t.Context())
	_, err = executor.ExecuteDropIndexConcurrently(uncancellable, pool, schema, "t", "i", anyIndexDefinition, executor.ConcurrentBudget{CallerOwned: true})
	require.ErrorIs(t, err, executor.ErrCallerOwnedNeedsCancellableContext)

	for _, names := range [][3]string{{"", "t", "i"}, {schema, "", "i"}, {schema, "t", ""}} {
		_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, names[0], names[1], names[2], anyIndexDefinition, dropBudget)
		require.ErrorIs(t, err, executor.ErrUnqualifiedTable, "%q", names)
	}

	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "i", "", dropBudget)
	require.ErrorIs(t, err, executor.ErrInvariantViolation, "a drop with no definition cannot be checked")
}

// DROP INDEX CONCURRENTLY never takes the table's ACCESS EXCLUSIVE lock: it
// waits for older transactions that could still use the index, and while it
// waits the table stays fully readable and writable. A transaction that has
// read the table holds the drop at that wait; during it another session
// reads and writes the table under a short lock_timeout, which a plain DROP
// INDEX queued for ACCESS EXCLUSIVE would have failed. When the old
// transaction ends, the drop completes.
func TestExecuteDropIndexConcurrentlyNeverBlocksTheTable(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int);
		CREATE INDEX t_a_idx ON %[1]s.t (a);
		INSERT INTO %[1]s.t VALUES (1, 1)`, schema))
	require.NoError(t, err)

	old := holdOldTransaction(t, pool, schema)

	type result struct {
		dropped executor.DroppedIndex
		err     error
	}
	done := make(chan result, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		dropped, dropErr := executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", reviewedDefinition(t, pool, schema, "t_a_idx"), dropBudget)
		done <- result{dropped: dropped, err: dropErr}
	})
	t.Cleanup(workers.Wait)

	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			  WHERE state = 'active' AND query ILIKE 'DROP INDEX CONCURRENTLY%' AND query LIKE '%' || $1 || '%')`,
			"t_a_idx").Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 20*time.Millisecond, "the drop must be waiting on the old transaction as DROP INDEX CONCURRENTLY")

	// The table is open to everyone else while the drop waits.
	other, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer other.Release()
	_, err = other.Exec(t.Context(), "SET lock_timeout = '200ms'")
	require.NoError(t, err)
	_, err = other.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (2, 2)", schema))
	require.NoError(t, err, "a write must not wait behind the drop")
	var n int
	require.NoError(t, other.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))
	assert.Equal(t, 2, n)
	var lockMode string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT coalesce(string_agg(DISTINCT mode, ','), '') FROM pg_locks
		  WHERE relation = $1::regclass AND mode = 'AccessExclusiveLock'`, schema+".t").Scan(&lockMode))
	assert.Empty(t, lockMode, "no session holds or waits for ACCESS EXCLUSIVE on the table")

	select {
	case res := <-done:
		require.Failf(t, "the drop finished while an older transaction was open", "%+v", res)
	default:
	}
	require.NoError(t, old.Rollback(t.Context()))
	res := <-done
	require.NoError(t, res.err)
	assert.Equal(t, "t_a_idx", res.dropped.Index)
	exists, _ := indexState(t, pool, schema, "t_a_idx")
	assert.False(t, exists)
}

// A drop that cannot finish inside its overall budget fails as a budget
// error. The index is left in place or invalid, which the diff reports as
// debris and never as a drop.
func TestExecuteDropIndexConcurrentlyBudgetExpiry(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, a int);
		CREATE INDEX t_a_idx ON %[1]s.t (a)`, schema))
	require.NoError(t, err)
	holdOldTransaction(t, pool, schema)

	tight := executor.ConcurrentBudget{Overall: 300 * time.Millisecond}
	_, err = executor.ExecuteDropIndexConcurrently(t.Context(), pool, schema, "t", "t_a_idx", reviewedDefinition(t, pool, schema, "t_a_idx"), tight)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseStatement, budgetErr.Cause)
	assert.Equal(t, tight.Overall, budgetErr.Budget)
	exists, _ := indexState(t, pool, schema, "t_a_idx")
	assert.True(t, exists, "a cancelled drop leaves the index entry for the operator, never half of one")
}
