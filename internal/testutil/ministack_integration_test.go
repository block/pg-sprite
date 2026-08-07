package testutil_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// TestAuroraControlPlaneProvisionAndConnect proves the AWS-boundary flow
// end to end: an aurora-postgresql cluster provisioned through the real RDS
// control-plane API is discoverable, its endpoint accepts connections
// through pkg/dbconn (bounded session defaults included), the server runs
// the requested PostgreSQL major, and DDL executes.
func TestAuroraControlPlaneProvisionAndConnect(t *testing.T) {
	url := testutil.ProvisionAuroraPostgres(t)

	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:         url,
		LockTimeout: 300 * time.Millisecond,
	})
	require.NoError(t, err, "connect to provisioned cluster endpoint via dbconn")
	t.Cleanup(pool.Close)

	// The server behind the endpoint runs the major the harness requested;
	// a silent fallback inside the emulator would invalidate the tier.
	requestedMajor, err := strconv.Atoi(testutil.PGVersion())
	require.NoError(t, err, "PG_VERSION must be a PostgreSQL major number")
	var versionNum int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int").Scan(&versionNum))
	assert.Equal(t, requestedMajor, versionNum/10000, "server major must match the requested major")

	// The dbconn bounded session settings apply on this endpoint like any
	// other PostgreSQL target.
	var lockTimeout string
	require.NoError(t, pool.QueryRow(t.Context(), "SHOW lock_timeout").Scan(&lockTimeout))
	assert.Equal(t, "300ms", lockTimeout)

	// DDL smoke: the provisioned database is writable and introspectable.
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), "CREATE TABLE "+schema+".t (id bigint PRIMARY KEY)")
	require.NoError(t, err, "create table on provisioned cluster")
	var oid *uint32
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass($1)::oid", schema+".t").Scan(&oid))
	assert.NotNil(t, oid, "created table must be visible in the catalog")
}
