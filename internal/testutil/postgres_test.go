package testutil_test

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// TestServerMajorMatchesRequestedVersion proves the harness tests what it
// claims: the server the suite connects to actually runs the PG_VERSION
// major the CI matrix selected. Without this, a matrix entry that silently
// fell back to a default image would still pass every test.
func TestServerMajorMatchesRequestedVersion(t *testing.T) {
	if os.Getenv("PG_DSN") != "" {
		t.Skip("PG_DSN points at an external server; version is not harness-selected")
	}
	url := testutil.StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var major string
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT (current_setting('server_version_num')::int / 10000)::text").Scan(&major))
	assert.Equal(t, testutil.PGVersion(), major,
		"server major must match the requested PG_VERSION")
}

// TestNewSchemaIsolation proves the per-test schema isolation the whole
// suite relies on: two schemas from NewSchema never collide, and objects
// created in one are invisible to the other.
func TestNewSchemaIsolation(t *testing.T) {
	url := testutil.StartPostgres(t)

	pool, err := pgxpool.New(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	s1 := testutil.NewSchema(t, pool)
	s2 := testutil.NewSchema(t, pool)
	require.NotEqual(t, s1, s2)
	require.True(t, strings.HasPrefix(s1, "t_"))

	_, err = pool.Exec(t.Context(), "CREATE TABLE "+s1+".only_here (id int)")
	require.NoError(t, err)

	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'only_here')",
		s2).Scan(&exists))
	assert.False(t, exists, "object in one throwaway schema must not appear in another")
}
