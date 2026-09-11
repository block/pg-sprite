package dbconn_test

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// Supavisor is exercised separately from the PgBouncer fixture: the wire
// protocol and handling of session settings are properties of each pooler.
func TestSupavisorSessionBoundary(t *testing.T) {
	sessionURL := os.Getenv("SUPABASE_SESSION_URL")
	transactionURL := os.Getenv("SUPABASE_TRANSACTION_URL")
	if sessionURL == "" && transactionURL == "" {
		t.Skip("start compose/supabase.yml services and set SUPABASE_SESSION_URL and SUPABASE_TRANSACTION_URL")
	}
	require.NotEmpty(t, sessionURL)
	require.NotEmpty(t, transactionURL)
	t.Run("session keeps execution bounds", func(t *testing.T) {
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: sessionURL, LockTimeout: 1500 * time.Millisecond, StatementTimeout: 7 * time.Second})
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		assert.Equal(t, int64(1500), sessionSetting(t, pool, "lock_timeout"))
		assert.Equal(t, int64(7000), sessionSetting(t, pool, "statement_timeout"))
	})
	t.Run("transaction cannot supply a stable session", func(t *testing.T) {
		// Supavisor transaction mode does not support named prepared statements.
		// Remove that independent protocol limitation to test the affinity guard.
		pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: transactionURL, QueryExecMode: pgx.QueryExecModeExec})
		if pool != nil {
			t.Cleanup(pool.Close)
		}
		require.ErrorIs(t, err, dbconn.ErrNoSessionAffinity)
	})
}
