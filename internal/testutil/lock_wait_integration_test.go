package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BlockedBy answers for the holder it names only. Two holders each take
// ACCESS EXCLUSIVE on a table of their own: while nothing waits, neither is
// blocking; a session queued behind the decoy's table makes the decoy
// blocking and leaves the other holder not, as another test's lock wait on
// a shared server would; and only a session queued behind the holder's own
// table makes the holder blocking.
func TestBlockedByNamesOnlyTheHoldersOwnWaiter(t *testing.T) {
	// The test holds four connections at once (two holders and two waiters)
	// and probes on a fifth, so the pool is sized for that rather than left
	// to its default, which is four on a four-CPU machine.
	cfg, err := pgxpool.ParseConfig(StartPostgres(t))
	require.NoError(t, err)
	cfg.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := NewSchema(t, pool)
	mine := pgx.Identifier{schema, "mine"}.Sanitize()
	other := pgx.Identifier{schema, "other"}.Sanitize()
	_, err = pool.Exec(t.Context(), `
		CREATE TABLE `+mine+` (id int);
		CREATE TABLE `+other+` (id int)`)
	require.NoError(t, err)

	holder := lockInTx(t, pool, `LOCK TABLE `+mine+` IN ACCESS EXCLUSIVE MODE`)
	decoy := lockInTx(t, pool, `LOCK TABLE `+other+` IN ACCESS EXCLUSIVE MODE`)
	assert.False(t, blockedBy(t, pool, holder), "a granted lock with no waiter blocks no one")

	queueBehind(t, pool, `LOCK TABLE `+other+` IN ACCESS EXCLUSIVE MODE`)
	const waitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		blocked, err := BlockedBy(t.Context(), pool, decoy)
		return err == nil && blocked
	}, waitDeadline, 10*time.Millisecond, "the decoy's table has a waiter")
	assert.False(t, blockedBy(t, pool, holder), "a wait on another table does not name this holder")

	queueBehind(t, pool, `LOCK TABLE `+mine+` IN ACCESS EXCLUSIVE MODE`)
	require.Eventually(t, func() bool {
		blocked, err := BlockedBy(t.Context(), pool, holder)
		return err == nil && blocked
	}, waitDeadline, 10*time.Millisecond, "the holder's own table has a waiter")
}

// lockInTx runs lock in a transaction held open for the rest of the test
// and returns its session's backend process ID.
func lockInTx(t *testing.T, pool *pgxpool.Pool, lock string) uint32 {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })
	_, err = tx.Exec(t.Context(), lock)
	require.NoError(t, err)
	return tx.Conn().PgConn().PID()
}

// queueBehind runs lock in a transaction of its own that waits until the
// test ends and its context is cancelled.
func queueBehind(t *testing.T, pool *pgxpool.Pool, lock string) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tx.Exec(t.Context(), lock)
	}()
	t.Cleanup(func() {
		<-done
		_ = tx.Rollback(context.WithoutCancel(t.Context()))
	})
}

func blockedBy(t *testing.T, pool *pgxpool.Pool, holder uint32) bool {
	t.Helper()
	blocked, err := BlockedBy(t.Context(), pool, holder)
	require.NoError(t, err)
	return blocked
}
