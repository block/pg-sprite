package testutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pin whose insert fails — here because pg_catalog has no such shadow,
// on any server — rolls its transaction back as it fails, so its
// connection is back in the pool before anyone ends the pin, and the
// cleanup reports the failure with the server's undefined_table SQLSTATE
// and no transaction to end.
func TestShadowPinRollsBackAFailedInsert(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), StartPostgres(t))
	require.NoError(t, err)
	pin := newShadowPin(t.Context(), pool, pgx.Identifier{"pg_catalog", "missing_shadow"}, 1, 0)

	pin.Clock().Now()
	// A connection the failed pin still held would make pool.Close wait on
	// it forever, so the pool is closed only once none is acquired.
	require.Zero(t, pool.Stat().AcquiredConns(), "the failed pin's connection is back in the pool")
	pool.Close()

	assert.Nil(t, pin.tx, "a failed insert leaves no pin to release")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, pin.end(context.WithoutCancel(t.Context())), &pgErr)
	assert.Equal(t, "42P01", pgErr.Code, "undefined_table: the shadow does not exist")
}

// newPinShadow creates the table a pin writes to, with the id and qty
// columns PinShadowKey expects, in a fresh schema.
func newPinShadow(t *testing.T, pool *pgxpool.Pool) pgx.Identifier {
	t.Helper()
	schema := NewSchema(t, pool)
	ExecInSchema(t, pool, schema, `
		CREATE TABLE shadow (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	return pgx.Identifier{schema, "shadow"}
}

// newPinPool returns a pool for the pin cleanup tests that fails instead
// of hanging when a pin is left open. Its sessions give up on a lock after
// pinLockTimeout, so the throwaway schema's drop does not wait on an open
// pin forever, and it is closed only when no connection is still acquired,
// since pool.Close waits on one without bound; the server's own cleanup
// ends that connection.
func newPinPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const pinLockTimeout = "5s"
	cfg, err := pgxpool.ParseConfig(StartPostgres(t))
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = pinLockTimeout
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		if pool.Stat().AcquiredConns() == 0 {
			pool.Close()
		}
	})
	return pool
}

// A pin still open when its test ends is rolled back by the cleanup: its
// connection returns to the pool and its row is never published.
func TestShadowPinCleanupRollsBackAnOpenPin(t *testing.T) {
	pool := newPinPool(t)
	shadow := newPinShadow(t, pool)

	t.Run("pins and walks away", func(t *testing.T) {
		pin := PinShadowKey(t, pool, shadow, 7, 1)
		pin.Clock().Now()
		pin.Wait(t)
		require.Equal(t, int32(1), pool.Stat().AcquiredConns(), "the open pin holds a connection")
	})

	assert.Zero(t, pool.Stat().AcquiredConns(), "the cleanup returned the pin's connection")
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+shadow.Sanitize()).Scan(&rows))
	assert.Zero(t, rows, "the pinned row was rolled back, not published")
}

// A cleanup that starts while the pin's hook is still placing the pin waits
// for the hook, then rolls the placed pin back. Another transaction holds
// key 7 uncommitted, so the pin's insert of the same key waits on it; the
// cleanup must not return until that insert has finished and been undone.
func TestShadowPinCleanupWaitsForAHookStillPlacingThePin(t *testing.T) {
	pool := newPinPool(t)
	shadow := newPinShadow(t, pool)

	// Cleanups run last-registered first: the holder lets go, the hook and
	// the cleanup goroutines finish, the pin is ended, then the pool closes.
	pin := newShadowPin(t.Context(), pool, shadow, 7, 1)
	t.Cleanup(func() { assert.NoError(t, pin.end(context.WithoutCancel(t.Context()))) })
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := holder.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back the holder: %v", err)
		}
	})
	_, err = holder.Exec(t.Context(), "INSERT INTO "+shadow.Sanitize()+" (id, qty) VALUES (7, 0)")
	require.NoError(t, err)

	wg.Go(func() { pin.Clock().Now() })
	const blockDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_stat_activity
				WHERE wait_event_type = 'Lock'
				  AND datname = pg_catalog.current_database()
			)`).Scan(&waiting)
		return err == nil && waiting
	}, blockDeadline, 20*time.Millisecond, "the pin's insert waits on the held key")

	ended := make(chan error, 1)
	wg.Go(func() { ended <- pin.end(context.WithoutCancel(t.Context())) })
	// A correct cleanup cannot return while the hook is blocked; an early
	// return shows up here, well inside this window.
	const earlyReturnWindow = 300 * time.Millisecond
	select {
	case err := <-ended:
		t.Errorf("the cleanup returned while the hook was still placing the pin: %v", err)
	case <-time.After(earlyReturnWindow):
	}

	require.NoError(t, holder.Rollback(t.Context()))
	const cleanupDeadline = 10 * time.Second
	select {
	case err := <-ended:
		require.NoError(t, err, "the cleanup rolls back the pin the hook placed")
	case <-time.After(cleanupDeadline):
		t.Fatalf("the cleanup did not return within %s of the hook finishing", cleanupDeadline)
	}

	assert.Zero(t, pool.Stat().AcquiredConns(), "the pin's connection is back in the pool")
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM "+shadow.Sanitize()).Scan(&rows))
	assert.Zero(t, rows, "neither the held row nor the pinned row was published")
}
