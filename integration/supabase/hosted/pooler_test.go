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
	f := newFixture(t)
	f.seedUnpublished(t)
	f.checkPoolerProject(t, dsn)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	pooled := *f
	pooled.pool = pool
	pooled.apply(t, "ALTER TABLE "+f.table+" ADD COLUMN session_note text")
	f.assertAccess(t)
}
func (f *fixture) checkPoolerProject(t *testing.T, dsn string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	ref := strings.TrimSuffix(strings.TrimPrefix(f.base, "https://"), ".supabase.co")
	require.Equal(t, "postgres."+ref, cfg.User, "pooler must address the same disposable project")
	require.True(t, strings.HasSuffix(cfg.Host, ".pooler.supabase.com"), "use the dashboard's Supavisor endpoint")
}
