//go:build ministack

package testutil_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
)

const (
	controlPlaneDeadline = 2 * time.Minute
	controlPlanePoll     = 250 * time.Millisecond
)

// TestAuroraControlPlane proves the AWS-boundary seams against one shared
// provisioned cluster: provisioning a cluster costs minutes, so the
// subtests share it rather than provisioning separately. They run in
// order, and PasswordRotation runs last because it changes the cluster's
// master password.
func TestAuroraControlPlane(t *testing.T) {
	cluster := testutil.ProvisionAuroraPostgres(t)

	t.Run("ProvisionAndConnect", func(t *testing.T) { provisionAndConnect(t, cluster) })
	t.Run("ErrorContract", func(t *testing.T) { errorContract(t, cluster) })
	t.Run("ReaderIsReadOnly", func(t *testing.T) { readerIsReadOnly(t, cluster) })
	t.Run("ConnectionLossDuringSchemaChange", func(t *testing.T) { connectionLossDuringSchemaChange(t, cluster) })
	t.Run("FailoverDuringSchemaChange", func(t *testing.T) { failoverDuringSchemaChange(t, cluster) })
	t.Run("PasswordRotation", func(t *testing.T) { passwordRotation(t, cluster) })
}

func newClusterPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:              url,
		LockTimeout:      300 * time.Millisecond,
		StatementTimeout: time.Minute,
		ConnectTimeout:   2 * time.Second,
	})
	require.NoError(t, err, "connect to provisioned cluster via dbconn")
	t.Cleanup(pool.Close)
	return pool
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
	var instanceExists *types.DBInstanceAlreadyExistsFault
	require.ErrorAs(t, err, &instanceExists,
		"creating a duplicate instance must surface the typed already-exists fault")
}

// readerIsReadOnly proves that Ministack's reader member is a real hot
// standby. The current preflight is catalog-only, so it succeeds there;
// PostgreSQL then refuses a write with read_only_sql_transaction, which the
// connection layer correctly treats as terminal rather than transient.
func readerIsReadOnly(t *testing.T, cluster *testutil.AuroraCluster) {
	writer := newClusterPool(t, cluster.URL())
	schema := testutil.NewSchema(t, writer)
	_, err := writer.Exec(t.Context(), "CREATE TABLE "+schema+".reader_probe (id bigint PRIMARY KEY)")
	require.NoError(t, err)

	reader := newClusterPool(t, cluster.ReaderURL())
	var inRecovery bool
	require.NoError(t, reader.QueryRow(t.Context(), "SELECT pg_is_in_recovery()").Scan(&inRecovery))
	assert.True(t, inRecovery, "reader instance must be a PostgreSQL hot standby")

	var proof preflight.PreflightedTable
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var checkErr error
		proof, checkErr = preflight.CheckTable(t.Context(), reader, schema, "reader_probe", preflight.NoSizeLimit)
		assert.NoError(collect, checkErr)
	}, controlPlaneDeadline, controlPlanePoll, "reader did not replay the table creation before the deadline")
	assert.Equal(t, "reader_probe", proof.Table())

	_, err = reader.Exec(t.Context(), "INSERT INTO "+schema+".reader_probe VALUES (1)")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a reader write must return a PostgreSQL server error")
	assert.Equal(t, "25006", pgErr.Code, "reader write must fail as read_only_sql_transaction")
	assert.False(t, dbconn.Retryable(err), "read-only standby writes are terminal")
}

// connectionLossDuringSchemaChange stops real cluster compute while DDL is
// executing. The optimistic executor has no connection-loss resume path: the
// connection layer does not retry an ambiguously interrupted write, and the
// executor's stable typed outcome is the fail-closed execution-failed fallback.
func connectionLossDuringSchemaChange(t *testing.T, cluster *testutil.AuroraCluster) {
	pool := newClusterPool(t, cluster.URL())
	schema := testutil.NewSchema(t, pool)
	ddl := "CREATE TABLE " + schema + ".interrupted AS SELECT 1 AS id FROM pg_sleep(300)"

	result := make(chan error, 1)
	go func() {
		_, err := pool.Exec(t.Context(), ddl)
		result <- err
	}()
	require.Eventuallyf(t, func() bool {
		var active bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE query LIKE $1 AND state = 'active' AND pid <> pg_backend_pid())`, "%"+schema+".interrupted%").Scan(&active)
		return err == nil && active
	}, controlPlaneDeadline, controlPlanePoll, "schema change did not become active before the deadline")

	stopped, err := cluster.Client.StopDBCluster(t.Context(), &rds.StopDBClusterInput{
		DBClusterIdentifier: aws.String(cluster.ClusterID),
	})
	require.NoError(t, err, "stop cluster during schema change")
	assert.Equal(t, "stopped", aws.ToString(stopped.DBCluster.Status))

	var changeErr error
	select {
	case changeErr = <-result:
	case <-time.After(controlPlaneDeadline):
		require.FailNow(t, "schema change did not return after cluster stop")
	}
	require.Error(t, changeErr)
	assert.False(t, dbconn.Retryable(changeErr),
		"an interrupted write with an ambiguous server outcome must not be retried")
	assert.Equal(t, executor.CodeExecutionFailed, executor.OutcomeCode(changeErr),
		"connection loss has no provable schema-change verdict and must fail closed")

	started, err := cluster.Client.StartDBCluster(t.Context(), &rds.StartDBClusterInput{
		DBClusterIdentifier: aws.String(cluster.ClusterID),
	})
	require.NoError(t, err, "restart cluster after connection loss")
	assert.Equal(t, "starting", aws.ToString(started.DBCluster.Status))
	require.Eventuallyf(t, func() bool {
		out, err := cluster.Client.DescribeDBClusters(t.Context(), &rds.DescribeDBClustersInput{
			DBClusterIdentifier: aws.String(cluster.ClusterID),
		})
		return err == nil && len(out.DBClusters) == 1 && aws.ToString(out.DBClusters[0].Status) == "available"
	}, controlPlaneDeadline, controlPlanePoll, "cluster did not become available after restart")

	pool.Reset()
	require.Eventuallyf(t, func() bool {
		_, err := pool.Exec(t.Context(), "CREATE TABLE "+schema+".after_restart (id bigint)")
		return err == nil
	}, controlPlaneDeadline, controlPlanePoll, "engine could not execute DDL after cluster restart")
}

// failoverDuringSchemaChange exercises only Ministack's current metadata
// failover: member writer flags flip and the response is transitional, but
// the standby is not promoted at the data plane yet. The established writer
// transaction therefore remains the engine's usable connection.
func failoverDuringSchemaChange(t *testing.T, cluster *testutil.AuroraCluster) {
	pool := newClusterPool(t, cluster.URL())
	schema := testutil.NewSchema(t, pool)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(t.Context()) }()
	_, err = tx.Exec(t.Context(), "CREATE TABLE "+schema+".during_failover (id bigint PRIMARY KEY)")
	require.NoError(t, err)

	failedOver, err := cluster.Client.FailoverDBCluster(t.Context(), &rds.FailoverDBClusterInput{
		DBClusterIdentifier:        aws.String(cluster.ClusterID),
		TargetDBInstanceIdentifier: aws.String(cluster.ReaderInstanceID),
	})
	require.NoError(t, err, "fail over cluster metadata")
	assert.Equal(t, "failing-over", aws.ToString(failedOver.DBCluster.Status))

	described, err := cluster.Client.DescribeDBClusters(t.Context(), &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(cluster.ClusterID),
	})
	require.NoError(t, err)
	require.Len(t, described.DBClusters, 1)
	writers := 0
	for _, member := range described.DBClusters[0].DBClusterMembers {
		if aws.ToBool(member.IsClusterWriter) {
			writers++
			assert.Equal(t, cluster.ReaderInstanceID, aws.ToString(member.DBInstanceIdentifier))
		}
	}
	assert.Equal(t, 1, writers, "metadata must expose exactly one writer")
	require.NoError(t, tx.Commit(t.Context()), "metadata-only failover must not interrupt the real writer")
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
	staleURL := cluster.URL()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:         staleURL,
		LockTimeout: 300 * time.Millisecond,
	})
	require.NoError(t, err, "connect with the pre-rotation password")
	t.Cleanup(pool.Close)
	held, err := pool.Acquire(t.Context())
	require.NoError(t, err, "check out a session before the rotation")
	var result int
	require.NoError(t, held.QueryRow(t.Context(), "SELECT 1").Scan(&result))

	cluster.Rotate(t)

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
