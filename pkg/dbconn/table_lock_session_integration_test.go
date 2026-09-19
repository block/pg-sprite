package dbconn

import (
	"context"
	"testing"
	"time"

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
