package hosted_test

import (
	"os"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// Pooler URLs come from Connect, not an inferred region or an assumed port.
func TestHostedSessionPooler(t *testing.T) {
	dsn := os.Getenv("SUPABASE_HOSTED_SESSION_URL")
	if dsn == "" {
		t.Skip("set SUPABASE_HOSTED_SESSION_URL to test the session pooler")
	}
	if os.Getenv("SUPABASE_HOSTED_TEST") != "1" {
		t.Skip("set SUPABASE_HOSTED_TEST=1 for a disposable hosted project")
	}
	checkPoolerProject(t, os.Getenv("SUPABASE_HOSTED_URL"), dsn)
	f := newFixture(t)
	f.seedUnpublished(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	pooled := *f
	pooled.pool = pool
	pooled.apply(t, "ALTER TABLE "+f.table+" ADD COLUMN session_note text")
	f.assertAccess(t)
}
func checkPoolerProject(t *testing.T, base, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	ref := strings.TrimSuffix(strings.TrimPrefix(base, "https://"), ".supabase.co")
	require.Equal(t, "postgres."+ref, cfg.User, "pooler must address the same disposable project")
	require.True(t, strings.HasSuffix(cfg.Host, ".pooler.supabase.com"), "use the dashboard's Supavisor endpoint")
}
