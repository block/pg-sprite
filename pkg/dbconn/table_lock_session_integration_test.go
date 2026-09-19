package dbconn

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// The keepalive must notice a session that is alive but no longer holds its
// lock; releasing it from inside the session needs the package-private conn.
func TestTableLockKeepaliveDetectsLockNotHeld(t *testing.T) {
	url := testutil.StartPostgres(t)
	session, err := AcquireTableLock(t.Context(), Config{URL: url}, "app", "unlocked_events",
		WithTableLockKeepalive(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.Error(t, session.Release(context.WithoutCancel(t.Context())))
	})

	session.opMu.Lock()
	_, unlockErr := session.conn.Exec(t.Context(), "SELECT pg_advisory_unlock_all()")
	session.opMu.Unlock()
	require.NoError(t, unlockErr)

	const lockLossDeadline = 10 * time.Second
	waitCtx, cancel := context.WithTimeout(t.Context(), lockLossDeadline)
	defer cancel()
	select {
	case <-session.Done():
		require.ErrorIs(t, session.Err(), ErrInvariantViolation)
	case <-waitCtx.Done():
		t.Fatalf("table lock loss was not detected within %s: %v", lockLossDeadline, waitCtx.Err())
	}
}

// A release that finds the advisory lock already gone is an invariant
// violation, not an ordinary error. The keepalive is long enough here that
// the monitor has not closed the session out from under Release.
func TestReleaseReportsALockThatWasNotHeld(t *testing.T) {
	url := testutil.StartPostgres(t)
	session, err := AcquireTableLock(t.Context(), Config{URL: url}, "app", "vanished",
		WithTableLockKeepalive(time.Hour))
	require.NoError(t, err)

	session.opMu.Lock()
	_, unlockErr := session.conn.Exec(t.Context(), "SELECT pg_advisory_unlock_all()")
	session.opMu.Unlock()
	require.NoError(t, unlockErr)

	require.ErrorIs(t, session.Release(context.WithoutCancel(t.Context())), ErrInvariantViolation)
}

// The dedicated session carries the same execution bounds as a pooled one
// and the same catalog-first search_path, so the acquire statement's
// unqualified hashtext cannot resolve to a decoy in a user schema (CO-9)
// and no statement on the session is unbounded (LK-2). BeforeConnect is the
// hook an operator's search_path arrives through, so it must apply here too.
func TestTableLockSessionIsPreparedLikeAPooledOne(t *testing.T) {
	url := testutil.StartPostgres(t)
	cfg := Config{
		URL:              url,
		LockTimeout:      2 * time.Second,
		StatementTimeout: 7 * time.Second,
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.RuntimeParams["search_path"] = "app, pg_catalog"
			return nil
		},
	}
	session, err := AcquireTableLock(t.Context(), cfg, "app", "prepared")
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, session.Release(context.WithoutCancel(t.Context())))
	})

	for _, bound := range sessionBounds(resolveTimeouts(cfg)) {
		var got int64
		require.NoError(t, session.conn.QueryRow(t.Context(),
			"SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = $1", bound.name).Scan(&got))
		assert.Equal(t, bound.ms, got, bound.name)
	}
	var searchPath string
	require.NoError(t, session.conn.QueryRow(t.Context(), "SHOW search_path").Scan(&searchPath))
	assert.Equal(t, "app", searchPath, "a pg_catalog entry behind a user schema is removed")
}
