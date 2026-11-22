package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachFixture is a partitioned table with the parent's ON ONLY index and a
// matching index on one partition, ready to attach.
type attachFixture struct {
	partitionedFixture
	parentIndex    string
	partitionIndex string
	parentTable    uint32
	partition      uint32
	other          uint32
}

func (f partitionedFixture) readyToAttach(t *testing.T) attachFixture {
	t.Helper()
	f.ordersByRegion(t)
	f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (total)`, f.schema)
	f.exec(t, `CREATE INDEX orders_eu_total ON %s.orders_eu (total)`, f.schema)
	return attachFixture{
		partitionedFixture: f,
		parentIndex:        "idx_orders_total",
		partitionIndex:     "orders_eu_total",
		parentTable:        f.tableOID(t, "orders"),
		partition:          f.tableOID(t, "orders_eu"),
		other:              f.tableOID(t, "orders_us"),
	}
}

func (f attachFixture) attachSQL() string {
	return fmt.Sprintf(`ALTER INDEX %s.%s ATTACH PARTITION %s.%s`, f.schema, f.parentIndex, f.schema, f.partitionIndex)
}

// begin opens a transaction on its own session with the given lock_timeout,
// so a statement that has to wait fails instead of hanging the test.
func (f partitionedFixture) begin(t *testing.T, lockTimeout string) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	_, err = tx.Exec(t.Context(), `SET LOCAL lock_timeout = '`+lockTimeout+`'`)
	require.NoError(t, err)
	return tx
}

// relationLocks reads the locks the transaction's own session holds on the
// named relations, as a relation name to lock mode map.
func (f partitionedFixture) relationLocks(t *testing.T, tx pgx.Tx, relations ...string) map[string]string {
	t.Helper()
	rows, err := tx.Query(t.Context(), `
		SELECT c.relname, l.mode
		  FROM pg_locks l
		  JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND l.granted
		   AND c.relnamespace = $1::regnamespace
		   AND c.relname = ANY($2)`, f.schema, relations)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, mode string
		require.NoError(t, rows.Scan(&name, &mode))
		out[name] = mode
	}
	require.NoError(t, rows.Err())
	return out
}

func requireSQLState(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	require.True(t, ok, "want a server error, got %v", err)
	assert.Equal(t, code, pgErr.Code, pgErr.Message)
}

// ALTER INDEX ... ATTACH PARTITION locks the partition's index, not only
// the parent's: it holds ACCESS EXCLUSIVE on the index being attached until
// its transaction ends, and only ACCESS SHARE on the two tables and SHARE
// UPDATE EXCLUSIVE on the parent's index. A statement opens the indexes of
// the table it reads or writes and releases them when it ends, so the attach
// waits behind DML statements that are running on that partition, not behind
// a writer's open transaction, and until it commits it holds back every new
// statement on that partition. The build's lock_timeout bounds how long the
// attach waits to acquire its locks; a statement held back by the attach
// waits under its own client's timeouts, until the attach's transaction ends.
// The other partitions are not affected.
// The modes are the same on every supported major.
func TestAttachPartitionLocksThePartitionsIndex(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		t.Run("the modes the attach holds", func(t *testing.T) {
			f := newFixture(t).readyToAttach(t)
			tx := f.begin(t, "1s")
			_, err := tx.Exec(t.Context(), f.attachSQL())
			require.NoError(t, err)

			locks := f.relationLocks(t, tx, "orders", "orders_eu", f.parentIndex, f.partitionIndex)

			assert.Equal(t, map[string]string{
				"orders":         "AccessShareLock",
				"orders_eu":      "AccessShareLock",
				f.parentIndex:    "ShareUpdateExclusiveLock",
				f.partitionIndex: "AccessExclusiveLock",
			}, locks)
		})

		t.Run("new statements on the partition wait for the attach and the other partitions do not", func(t *testing.T) {
			f := newFixture(t).readyToAttach(t)
			attach := f.begin(t, "1s")
			_, err := attach.Exec(t.Context(), f.attachSQL())
			require.NoError(t, err)

			for _, c := range []struct{ name, sql string }{
				{"an insert", fmt.Sprintf(`INSERT INTO %s.orders_eu VALUES (1000, 'eu', 1, 'n')`, f.schema)},
				{"a read", fmt.Sprintf(`SELECT count(*) FROM %s.orders_eu`, f.schema)},
			} {
				t.Run(c.name, func(t *testing.T) {
					blocked := f.begin(t, "250ms")
					_, err := blocked.Exec(t.Context(), c.sql)
					requireSQLState(t, err, "55P03")
				})
			}

			free := f.begin(t, "250ms")
			_, err = free.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.orders_us VALUES (1000, 'us', 1, 'n')`, f.schema))
			require.NoError(t, err)
		})

		// A writer's open transaction holds the table's ROW EXCLUSIVE lock
		// and no lock on the index, so it does not delay the attach.
		t.Run("an open writer transaction does not delay the attach", func(t *testing.T) {
			f := newFixture(t).readyToAttach(t)
			writer := f.begin(t, "1s")
			_, err := writer.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.orders_eu VALUES (1000, 'eu', 1, 'n')`, f.schema))
			require.NoError(t, err)

			attach := f.begin(t, "250ms")
			_, err = attach.Exec(t.Context(), f.attachSQL())

			require.NoError(t, err)
		})

		t.Run("a statement running on the partition delays the attach until the lock timeout", func(t *testing.T) {
			f := newFixture(t).readyToAttach(t)
			// The statement opens the partition's indexes when it starts,
			// inserts one row, and then sleeps while it evaluates the next.
			conn, err := f.pool.Acquire(t.Context())
			require.NoError(t, err)
			pid := conn.Conn().PgConn().PID()
			writerCtx, stop := context.WithCancel(t.Context())
			finished := make(chan error, 1)
			go func() {
				_, err := conn.Exec(writerCtx, fmt.Sprintf(
					`INSERT INTO %s.orders_eu SELECT 1000 + g, 'eu', 1, 'n' FROM generate_series(1, 2) g WHERE g = 1 OR pg_sleep(20) IS NOT NULL`, f.schema))
				finished <- err
			}()
			t.Cleanup(func() {
				stop()
				assert.Error(t, <-finished, "the running statement is cancelled when the test ends")
				conn.Release()
			})
			require.Eventually(t, func() bool {
				var sleeping bool
				require.NoError(t, f.pool.QueryRow(t.Context(), `
					SELECT COALESCE((SELECT wait_event = 'PgSleep' FROM pg_stat_activity WHERE pid = $1), false)`, pid).Scan(&sleeping))
				return sleeping
			}, 30*time.Second, 20*time.Millisecond, "the insert must be sleeping inside its statement")

			attach := f.begin(t, "250ms")
			_, err = attach.Exec(t.Context(), f.attachSQL())

			requireSQLState(t, err, "55P03")
		})
	})
}
