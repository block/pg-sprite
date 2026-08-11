//go:build ministack

package testutil_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// TestAuroraControlPlane proves the AWS-boundary seams against one shared
// provisioned cluster: provisioning a cluster costs minutes, so the
// subtests share it rather than provisioning three times. They run in
// order, and PasswordRotation runs last because it changes the cluster's
// master password.
func TestAuroraControlPlane(t *testing.T) {
	cluster := testutil.ProvisionAuroraPostgres(t)

	t.Run("ProvisionAndConnect", func(t *testing.T) { provisionAndConnect(t, cluster) })
	t.Run("ErrorContract", func(t *testing.T) { errorContract(t, cluster) })
	t.Run("PasswordRotation", func(t *testing.T) { passwordRotation(t, cluster) })
}

// provisionAndConnect proves the AWS-boundary flow end to end: an
// aurora-postgresql cluster provisioned through the real RDS control-plane
// API is discoverable, its endpoint accepts connections through pkg/dbconn
// (bounded session defaults included), the server runs the requested
// PostgreSQL major, and DDL executes.
func provisionAndConnect(t *testing.T, cluster *testutil.AuroraCluster) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:         cluster.URL(),
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

// errorContract proves the control-plane error contract the engine's
// discovery code will rely on: unknown identifiers and duplicate creations
// surface as the AWS SDK's typed RDS faults, matchable with errors.As —
// never by message text.
func errorContract(t *testing.T, cluster *testutil.AuroraCluster) {
	ctx := t.Context()

	_, err := cluster.Client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String("pgsprite-does-not-exist"),
	})
	var clusterNotFound *types.DBClusterNotFoundFault
	require.ErrorAs(t, err, &clusterNotFound,
		"describing an unknown cluster must surface the typed not-found fault")

	_, err = cluster.Client.CreateDBCluster(ctx, &rds.CreateDBClusterInput{
		DBClusterIdentifier: aws.String(cluster.ClusterID),
		Engine:              aws.String("aurora-postgresql"),
		MasterUsername:      aws.String("pgsprite"),
		MasterUserPassword:  aws.String("test-password-do-not-use"),
	})
	var clusterExists *types.DBClusterAlreadyExistsFault
	require.ErrorAs(t, err, &clusterExists,
		"creating a duplicate cluster must surface the typed already-exists fault")

	_, err = cluster.Client.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(cluster.InstanceID),
		DBClusterIdentifier:  aws.String(cluster.ClusterID),
		Engine:               aws.String("aurora-postgresql"),
		DBInstanceClass:      aws.String("db.t3.medium"),
	})
	// Real AWS emits wire code "DBInstanceAlreadyExists", which the SDK
	// maps to types.DBInstanceAlreadyExistsFault — production code must
	// match that typed fault with errors.As, exactly like the two cases
	// above. Ministack diverges: it emits "DBInstanceAlreadyExistsFault",
	// which the SDK leaves as a generic API error. Pin the divergent code
	// exactly so this assertion fails the day the emulator is fixed, and
	// this workaround is replaced by the typed errors.As match.
	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr,
		"creating a duplicate instance must surface an RDS API error")
	require.Equal(t, "DBInstanceAlreadyExistsFault", apiErr.ErrorCode(),
		"emulator no longer emits its divergent duplicate-instance code — assert types.DBInstanceAlreadyExistsFault with errors.As instead of this pin")
}

// passwordRotation proves what a master-password rotation does to a
// running schema change, and pins pg-sprite's contract for the failure.
// PostgreSQL never re-authenticates an established session, so in-flight
// work keeps running through the rotation; the stale credentials fail on
// the next dial — a pool recycle, growth past the idle set, or a
// reconnect — with SQLSTATE 28P01, which pg-sprite classifies as
// terminal: one clean failure, never a retry storm against an
// auth-failing endpoint.
func passwordRotation(t *testing.T, cluster *testutil.AuroraCluster) {
	// A pool dialed with the pre-rotation password, with one session
	// checked out — a schema change in flight.
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:         cluster.URL(),
		LockTimeout: 300 * time.Millisecond,
	})
	require.NoError(t, err, "connect with the pre-rotation password")
	t.Cleanup(pool.Close)
	held, err := pool.Acquire(t.Context())
	require.NoError(t, err, "check out a session before the rotation")
	var result int
	require.NoError(t, held.QueryRow(t.Context(), "SELECT 1").Scan(&result))

	const rotatedPassword = "test-password-rotated-do-not-use"
	cluster.Rotate(t, rotatedPassword)

	// The established session sails through the rotation: PostgreSQL
	// authenticates at connection time only.
	require.NoError(t, held.QueryRow(t.Context(), "SELECT 2").Scan(&result),
		"an established session must keep working through a rotation")
	assert.Equal(t, 2, result)
	held.Release()

	// The failure lands on the next dial. Reset stands in for the ways a
	// pool re-dials in production — MaxConnLifetime expiry, growth past
	// the idle set, a reconnect after a network blip.
	pool.Reset()
	_, err = pool.Acquire(t.Context())
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a dial with the stale password must fail with a server auth error")
	assert.Equal(t, "28P01", pgErr.Code, "stale password must be refused as invalid_password")

	// pg-sprite's own contract for that failure: auth errors are terminal,
	// not transient — the engine surfaces one clean failure instead of
	// retrying against an endpoint that will keep refusing it.
	assert.False(t, dbconn.Retryable(err), "an auth failure must not be classified as retryable")

	// The rotated credentials connect through dbconn; the handle's URL
	// reflects them after Rotate.
	fresh, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:         cluster.URL(),
		LockTimeout: 300 * time.Millisecond,
	})
	require.NoError(t, err, "connect with the rotated password")
	fresh.Close()
}
