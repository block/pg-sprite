package dbconn

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// The read-back is a check in its own right, not a restatement of the
// affinity proof. The proof catches a connection that does not keep one
// session; this catches a session that kept everything it was given and was
// never given the bounds. Both end in a refusal because a session without
// lock_timeout is LK-2 gone: an ALTER that queues sits at the head of the
// lock queue indefinitely, blocking every reader and writer behind it.
func TestVerifySessionSettingsRefusesASessionThatNeverReceivedTheBounds(t *testing.T) {
	pc, err := buildPoolConfig(Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	// Remove both routes the bounds take to a session — the startup
	// parameters and the SET hook — leaving a session as unbounded as a
	// pooler that drops what it is sent would leave it.
	delete(pc.ConnConfig.RuntimeParams, "lock_timeout")
	delete(pc.ConnConfig.RuntimeParams, "statement_timeout")
	pc.AfterConnect = unshadowCatalog

	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	err = verifySessionSettings(t.Context(), pool, sessionBounds(3*time.Second, 30*time.Second))

	require.ErrorIs(t, err, ErrSessionSettingsDiscarded)
	assert.Contains(t, err.Error(), "lock_timeout is unbounded",
		"the refusal names the bound that is missing and what its value means")
	assert.Contains(t, err.Error(), "session-mode endpoint",
		"the refusal names what an operator changes")
}

// A session that carries the bounds passes, so the check refuses a missing
// bound rather than refusing everything.
func TestVerifySessionSettingsAcceptsASessionCarryingTheBounds(t *testing.T) {
	pc, err := buildPoolConfig(Config{
		URL:              testutil.StartPostgres(t),
		LockTimeout:      3 * time.Second,
		StatementTimeout: 30 * time.Second,
	})
	require.NoError(t, err)

	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, verifySessionSettings(t.Context(), pool, sessionBounds(3*time.Second, 30*time.Second)))
}
