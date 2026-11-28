package schemachange

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The swap follows a lost attempt by its backend's PID and start, read in
// the swap's own transaction. A pool whose sessions log in as the
// superuser and run as a role without pg_read_all_stats reads its own
// start as NULL, so the read refuses with dbconn.ErrSessionStartHidden
// rather than failing to scan the NULL. The same read on a session that
// logged in as the role it runs as returns its PID and a start.
func TestReadSwapBackendRefusesASessionThatCannotReadItsOwnStart(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	role := testutil.NewRole(t, admin, "NOLOGIN")
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	query := u.Query()
	query.Set("role", role)
	u.RawQuery = query.Encode()
	switched, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(switched.Close)

	t.Run("a switched role", func(t *testing.T) {
		backend, err := readSwapBackendIn(t, switched)
		require.ErrorIs(t, err, dbconn.ErrSessionStartHidden)
		assert.Zero(t, backend)
	})

	t.Run("the login role", func(t *testing.T) {
		backend, err := readSwapBackendIn(t, admin)
		require.NoError(t, err)
		assert.NotZero(t, backend.pid)
		assert.False(t, backend.started.IsZero())
	})
}

// readSwapBackendIn runs readSwapBackend in a transaction of its own on
// pool, rolled back afterwards.
func readSwapBackendIn(t *testing.T, pool *pgxpool.Pool) (swapBackend, error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	}()
	return readSwapBackend(t.Context(), tx)
}
