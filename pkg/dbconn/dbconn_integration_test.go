package dbconn_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestPoolIntegration(t *testing.T) {
	url := testutil.StartPostgres(t)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:              url,
		LockTimeout:      300 * time.Millisecond,
		StatementTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("session timeouts are applied", func(t *testing.T) {
		var lockTimeout, stmtTimeout string
		require.NoError(t, pool.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&lockTimeout))
		require.NoError(t, pool.QueryRow(t.Context(), "SHOW statement_timeout").Scan(&stmtTimeout))
		assert.Equal(t, "300ms", lockTimeout)
		assert.Equal(t, "500ms", stmtTimeout)
	})

	t.Run("statement_timeout cancels runaway work", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), "SELECT pg_sleep(5)")
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "57014", pgErr.Code, "expected query_canceled from statement_timeout")
	})

	t.Run("lock wait beyond lock_timeout is a retryable error", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		table := schema + ".locked"
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+table+" (id int primary key)")
		require.NoError(t, err)

		holder, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer holder.Release()
		tx, err := holder.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(t.Context()) }()
		_, err = tx.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
		require.NoError(t, err)

		_, err = pool.Exec(t.Context(), "INSERT INTO "+table+" VALUES (1)")
		require.Error(t, err)
		assert.True(t, dbconn.Retryable(err), "lock_not_available must classify as retryable, got: %v", err)
	})

	t.Run("TerminateBlockers evicts exactly the blocking backend", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		table := schema + ".swap_target"
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+table+" (id int primary key)")
		require.NoError(t, err)

		// Backend A holds ACCESS EXCLUSIVE in an open transaction.
		connA, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer connA.Release()
		var aPid int
		require.NoError(t, connA.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&aPid))
		txA, err := connA.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = txA.Rollback(t.Context()) }()
		_, err = txA.Exec(t.Context(), "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
		require.NoError(t, err)

		// Backend B queues behind A with a generous lock_timeout.
		connB, err := pool.Acquire(t.Context())
		require.NoError(t, err)
		defer connB.Release()
		var bPid int
		require.NoError(t, connB.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&bPid))
		_, err = connB.Exec(t.Context(), "SET lock_timeout = '30s'")
		require.NoError(t, err)
		insertDone := make(chan error, 1)
		go func() {
			_, err := connB.Exec(t.Context(), "INSERT INTO "+table+" VALUES (1)")
			insertDone <- err
		}()

		const blockedDeadline = 10 * time.Second
		require.Eventually(t, func() bool {
			var blocked bool
			err := pool.QueryRow(t.Context(),
				"SELECT cardinality(pg_blocking_pids($1::int)) > 0", bPid).Scan(&blocked)
			return err == nil && blocked
		}, blockedDeadline, 50*time.Millisecond, "backend B never showed up as blocked behind A")

		terminated, err := dbconn.TerminateBlockers(t.Context(), pool, bPid)
		require.NoError(t, err)
		assert.Equal(t, []int{aPid}, terminated, "only the holder should be terminated")

		const insertDeadline = 10 * time.Second
		select {
		case err := <-insertDone:
			require.NoError(t, err, "B's insert should succeed once the blocker is evicted")
		case <-time.After(insertDeadline):
			t.Fatalf("B's insert still blocked %s after terminating the blocker", insertDeadline)
		}
	})
}
