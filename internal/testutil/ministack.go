package testutil

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The Ministack tier proves the AWS boundary — the RDS/Aurora control
// plane (cluster and instance provisioning, endpoint discovery) — not
// Aurora data-plane behavior: the database Ministack hands back is real
// vanilla PostgreSQL running in a sibling container. Core DDL logic stays
// on the real-PostgreSQL harness in postgres.go; Aurora-only semantics
// need a real cluster (see docs/testing.md).
const (
	// ministackGatewayPort is Ministack's edge port for all AWS APIs.
	ministackGatewayPort = 4566
	// auroraProvisionDeadline bounds how long a provisioned cluster may take
	// to become available. The first provision pulls the postgres image for
	// the sibling database container, which dominates this budget.
	auroraProvisionDeadline = 5 * time.Minute
	auroraProvisionPoll     = 2 * time.Second
	// rdsStatusAvailable is the RDS API status of a usable instance.
	rdsStatusAvailable = "available"
	// dockerSocket is mounted into the Ministack container so it can start
	// the sibling PostgreSQL container that backs the provisioned cluster.
	// This grants the emulator access to the host Docker daemon — fine for
	// a test harness, but the reason this tier must never run against a
	// shared Docker host it does not own.
	dockerSocket = "/var/run/docker.sock"
	// fixtureUser, fixturePassword, and fixtureDatabase are emulator-only
	// test fixtures, never real credentials: Ministack hands them to the
	// sibling database container it creates for the cluster.
	fixtureUser     = "pgsprite"
	fixturePassword = "test-password-do-not-use"
	fixtureDatabase = "pgsprite"
)

// ministackImage returns the Ministack image to run, pinned for
// reproducibility; MINISTACK_IMAGE overrides it for upgrades or registry
// mirrors. The "full" edition ships native database drivers, so the
// emulator marks an instance available only after an authenticated probe
// query succeeds — the slim edition would fall back to a TCP check that
// can pass before PostgreSQL accepts logins.
func ministackImage() string {
	if img := os.Getenv("MINISTACK_IMAGE"); img != "" {
		return img
	}
	return "ministackorg/ministack:1.4.13-full"
}

// ProvisionAuroraPostgres starts a Ministack container, provisions an
// aurora-postgresql cluster and instance through the real RDS control-plane
// API, waits until the instance is available, and returns a connection URL
// for the cluster's database. The PostgreSQL major follows PG_VERSION: the
// cluster's database is a real postgres container of that major.
func ProvisionAuroraPostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs Docker")
	}

	ctx := t.Context()
	// The sibling database container publishes its port on the Docker host
	// starting at RDS_BASE_PORT. Pinning that base to a port this process
	// picked keeps the database reachable at a known localhost address on
	// every platform — container IPs are not routable from the host on
	// macOS, so the endpoint address the API returns cannot be used
	// directly.
	dbPort := freePort(t)
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        ministackImage(),
			ExposedPorts: []string{fmt.Sprintf("%d/tcp", ministackGatewayPort)},
			Env: map[string]string{
				"RDS_BASE_PORT": strconv.Itoa(dbPort),
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, dockerSocket+":"+dockerSocket)
			},
			WaitingFor: wait.ForHTTP("/_ministack/health").
				WithPort(fmt.Sprintf("%d/tcp", ministackGatewayPort)),
		},
	})
	require.NoError(t, err, "start Ministack container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate Ministack container: %v", err)
		}
	})

	client := rdsClient(t, ctr)
	major, err := strconv.Atoi(PGVersion())
	require.NoError(t, err, "PG_VERSION must be a PostgreSQL major number")

	const clusterID = "pgsprite-test"
	_, err = client.CreateDBCluster(ctx, &rds.CreateDBClusterInput{
		DBClusterIdentifier: aws.String(clusterID),
		Engine:              aws.String("aurora-postgresql"),
		EngineVersion:       aws.String(strconv.Itoa(major)),
		DatabaseName:        aws.String(fixtureDatabase),
		MasterUsername:      aws.String(fixtureUser),
		MasterUserPassword:  aws.String(fixturePassword),
	})
	require.NoError(t, err, "create aurora-postgresql cluster")
	// The database backing the cluster is a sibling Docker container, not a
	// child of the Ministack container — terminating Ministack alone would
	// leak it. Deleting the cluster through the API reaps it. Cleanups run
	// last-in-first-out, so the instance delete registered below runs
	// before this — matching the RDS rule that a cluster cannot be deleted
	// while it still has instances.
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := client.DeleteDBCluster(cleanupCtx, &rds.DeleteDBClusterInput{
			DBClusterIdentifier: aws.String(clusterID),
			SkipFinalSnapshot:   aws.Bool(true),
		}); err != nil {
			t.Logf("delete cluster %s: %v", clusterID, err)
		}
	})

	instanceID := clusterID + "-1"
	_, err = client.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(instanceID),
		DBClusterIdentifier:  aws.String(clusterID),
		Engine:               aws.String("aurora-postgresql"),
		DBInstanceClass:      aws.String("db.t3.medium"),
	})
	require.NoError(t, err, "create aurora-postgresql instance")
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := client.DeleteDBInstance(cleanupCtx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(instanceID),
			SkipFinalSnapshot:    aws.Bool(true),
		}); err != nil {
			t.Logf("delete instance %s: %v", instanceID, err)
		}
	})

	require.Eventuallyf(t, func() bool {
		out, err := client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(instanceID),
		})
		if err != nil || len(out.DBInstances) == 0 {
			return false
		}
		return aws.ToString(out.DBInstances[0].DBInstanceStatus) == rdsStatusAvailable
	}, auroraProvisionDeadline, auroraProvisionPoll,
		"instance %s did not become %s within the provision deadline", instanceID, rdsStatusAvailable)

	// Read the endpoint back from the control plane rather than trusting the
	// request: the discovery flow is the behavior under test.
	clusters, err := client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(clusterID),
	})
	require.NoError(t, err, "describe cluster after provisioning")
	require.Len(t, clusters.DBClusters, 1, "provisioned cluster must be discoverable")
	require.NotEmpty(t, aws.ToString(clusters.DBClusters[0].Endpoint),
		"cluster endpoint address must be discoverable")
	require.NotZero(t, aws.ToInt32(clusters.DBClusters[0].Port),
		"cluster endpoint port must be discoverable")

	// The discovered endpoint address is container-internal; connect via the
	// pinned host-published port instead (see dbPort above).
	//
	// sslmode=disable: the sibling database container runs plain PostgreSQL
	// without TLS, and the endpoint is not an *.rds.amazonaws.com hostname,
	// so the production TLS path is out of scope for this tier (it is
	// proven by pkg/dbconn's TLS integration tests).
	return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable",
		fixtureUser, fixturePassword, dbPort, fixtureDatabase)
}

// freePort reserves an ephemeral TCP port and returns it for reuse. The
// port is released before returning, so a collision is possible but
// unlikely within a test's lifetime.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "reserve a free TCP port")
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close(), "release the reserved port")
	return port
}

// rdsClient returns an RDS API client pointed at the container's gateway
// with the emulator's conventional static test credentials.
func rdsClient(t *testing.T, ctr testcontainers.Container) *rds.Client {
	t.Helper()
	ctx := t.Context()
	host, err := ctr.Host(ctx)
	require.NoError(t, err, "resolve container host")
	gateway, err := ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", ministackGatewayPort))
	require.NoError(t, err, "resolve mapped gateway port")
	endpoint := fmt.Sprintf("http://%s:%d", host, gateway.Num())

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err, "load AWS SDK config")
	return rds.NewFromConfig(cfg, func(o *rds.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
}
