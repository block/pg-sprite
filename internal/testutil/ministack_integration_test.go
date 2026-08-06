package testutil_test

import (
	"strconv"
	"strings"
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

// TestAuroraControlPlaneProvisionAndConnect proves the AWS-boundary flow
// end to end: an aurora-postgresql cluster provisioned through the real RDS
// control-plane API is discoverable, its endpoint accepts connections
// through pkg/dbconn (bounded session defaults included), the server runs
// the requested PostgreSQL major, and DDL executes.
func TestAuroraControlPlaneProvisionAndConnect(t *testing.T) {
	cluster := testutil.ProvisionAuroraPostgres(t)

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

// TestAuroraControlPlaneErrorContract proves the control-plane error
// contract the engine's discovery code will rely on: unknown identifiers
// and duplicate creations surface as the AWS SDK's typed RDS faults,
// matchable with errors.As — never by message text.
func TestAuroraControlPlaneErrorContract(t *testing.T) {
	cluster := testutil.ProvisionAuroraPostgres(t)
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
	// Real AWS emits wire code "DBInstanceAlreadyExists" (which the SDK maps
	// to types.DBInstanceAlreadyExistsFault); Ministack emits
	// "DBInstanceAlreadyExistsFault", which the SDK leaves as a generic API
	// error. Match the code prefix so the assertion holds against both the
	// emulator and a real endpoint — engine code consuming this error must
	// do the same.
	var apiErr smithy.APIError
	require.ErrorAs(t, err, &apiErr,
		"creating a duplicate instance must surface an RDS API error")
	assert.True(t, strings.HasPrefix(apiErr.ErrorCode(), "DBInstanceAlreadyExists"),
		"duplicate instance error code must identify the already-exists fault, got %q", apiErr.ErrorCode())
}

// TestAuroraControlPlanePasswordRotation proves the rotation seam the
// credential test theme builds on: ModifyDBCluster applies a new master
// password to the running database, after which the stale password is
// refused with SQLSTATE 28P01 (invalid_password) and the new one connects.
func TestAuroraControlPlanePasswordRotation(t *testing.T) {
	// Named polling bounds for the rotation to land on the real database.
	const (
		rotationDeadline = time.Minute
		rotationPoll     = time.Second
	)

	cluster := testutil.ProvisionAuroraPostgres(t)
	ctx := t.Context()

	const rotatedPassword = "test-password-rotated-do-not-use"
	_, err := cluster.Client.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier: aws.String(cluster.ClusterID),
		MasterUserPassword:  aws.String(rotatedPassword),
		ApplyImmediately:    aws.Bool(true),
	})
	require.NoError(t, err, "rotate master password via ModifyDBCluster")

	// The rotation must land on the real database, not just the metadata:
	// poll until the new password opens a connection through dbconn.
	require.Eventuallyf(t, func() bool {
		pool, err := dbconn.NewPool(ctx, dbconn.Config{
			URL:         cluster.URLWithPassword(rotatedPassword),
			LockTimeout: 300 * time.Millisecond,
		})
		if err != nil {
			return false
		}
		pool.Close()
		return true
	}, rotationDeadline, rotationPoll,
		"rotated master password did not become usable within the deadline")

	// The stale password is refused by authentication — the exact failure
	// a mid-migration connection hits after a production rotation.
	_, err = dbconn.NewPool(ctx, dbconn.Config{
		URL:         cluster.URL(),
		LockTimeout: 300 * time.Millisecond,
	})
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "stale password must fail with a server auth error")
	assert.Equal(t, "28P01", pgErr.Code, "stale password must be refused as invalid_password")
}
