//go:build ministack

// The ministack build tag confines this harness — and the AWS SDK it pulls
// in — to explicit opt-in via `make test-aws-boundary`. A plain `go test
// ./...` never compiles it, so the default suite needs no Docker-socket
// mount and the AWS SDK stays out of every ordinary build.

package testutil

import (
	"context"
	"crypto/sha1" // not cryptographic: reproduces Ministack's container-name derivation
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
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
	// rotationDeadline bounds how long a rotated master password may take
	// to land on the running database after managed rotation returns.
	rotationDeadline = time.Minute
	rotationPoll     = time.Second
	// rdsStatusAvailable is the RDS API status of a usable instance.
	rdsStatusAvailable = "available"
	// endpointDialTimeout bounds the reachability probe of the discovered
	// endpoint. The instance is already available — Ministack marks it so
	// only after an authenticated probe query succeeds — so a reachable
	// address accepts immediately and a timeout means a routing gap, not a
	// database that is still starting.
	endpointDialTimeout = 3 * time.Second
	// siblingDBPort is the PostgreSQL port inside the sibling database
	// container, which Ministack also publishes on the Docker host.
	siblingDBPort = "5432/tcp"
	// dockerSocket is mounted into the Ministack container so it can start
	// the sibling PostgreSQL container that backs the provisioned cluster.
	// This grants the emulator access to the host Docker daemon — fine for
	// a test harness, but the reason this tier must never run against a
	// shared Docker host it does not own.
	dockerSocket = "/var/run/docker.sock"
	// fixtureUser and fixtureDatabase are emulator-only test fixtures:
	// Ministack hands them to the sibling database container it creates for
	// the cluster. The master password is generated and managed by RDS.
	fixtureUser     = "pgsprite"
	fixtureDatabase = "pgsprite"
	// awsAccountID and awsRegion identify the emulator's default account.
	// Ministack scopes the sibling container's name by
	// sha1(account:region), so these also feed siblingHostAddr.
	awsAccountID = "000000000000"
	awsRegion    = "us-east-1"
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
	return "ministackorg/ministack:1.4.15-full"
}

// auroraEngineVersion returns a real aurora-postgresql engine version for
// the requested major. Real RDS requires a full version string ("16.14"),
// not a bare major, and Ministack validates the version against its
// creatable catalog at create time — an unknown version fails with the
// AWS-exact InvalidParameterCombination, so each entry here must be a
// version the pinned image advertises via DescribeDBEngineVersions. The
// exact minor is immaterial beyond that: Ministack derives the sibling
// database image from the major, and the test asserts the running
// server's major independently.
func auroraEngineVersion(major int) string {
	versions := map[int]string{
		14: "14.23",
		15: "15.18",
		16: "16.14",
		17: "17.10",
		18: "18.4",
	}
	if v, ok := versions[major]; ok {
		return v
	}
	// A major newer than this map: fall back to the bare major, which the
	// emulator's validator accepts as a dot-boundary prefix of any
	// catalog entry for that major.
	return strconv.Itoa(major)
}

// AuroraCluster is a provisioned Ministack aurora-postgresql cluster and
// the control-plane client that owns it. Tests drive further control-plane
// operations (rotation, duplicate creation, discovery of unknown
// identifiers) through Client against ClusterID and InstanceID.
type AuroraCluster struct {
	// Client is the RDS control-plane client bound to the Ministack gateway.
	Client *rds.Client
	// ClusterID is the DBClusterIdentifier of the provisioned cluster.
	ClusterID string
	// InstanceID is the DBInstanceIdentifier of the cluster's sole instance.
	InstanceID string

	// addr is the host:port the test connects to — the discovered cluster
	// endpoint when reachable, otherwise the sibling container's
	// host-published address (see ProvisionAuroraPostgres).
	addr string
	// password is the master password the cluster currently accepts.
	// Rotate keeps it in sync with the control plane so URL never goes
	// silently stale after a rotation.
	password string
	// secrets is the Secrets Manager client used to resolve the RDS-managed
	// master password through the same gateway.
	secrets *secretsmanager.Client
}

// URL returns a connection URL for the cluster's database using the
// master password the cluster currently accepts. After Rotate, that is
// the rotated password.
func (c *AuroraCluster) URL() string {
	return c.URLWithPassword(c.password)
}

// URLWithPassword returns a connection URL using the given master
// password — for tests that deliberately present stale or wrong
// credentials.
//
// sslmode=disable: the sibling database container runs plain PostgreSQL
// without TLS, and the endpoint is not an *.rds.amazonaws.com hostname,
// so the production TLS path is out of scope for this tier (it is
// proven by pkg/dbconn's TLS integration tests).
func (c *AuroraCluster) URLWithPassword(password string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		fixtureUser, password, c.addr, fixtureDatabase)
}

// Rotate asks RDS to generate a new managed master password, resolves it
// from Secrets Manager, waits until the running database accepts it, and
// updates the handle so URL reflects the credentials the cluster now accepts.
func (c *AuroraCluster) Rotate(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	_, err := c.Client.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
		DBClusterIdentifier:      aws.String(c.ClusterID),
		RotateMasterUserPassword: aws.Bool(true),
		ApplyImmediately:         aws.Bool(true),
	})
	require.NoError(t, err, "rotate RDS-managed master password")

	// Rotation and the secret write are the control plane's to sequence:
	// poll until the secret no longer resolves to the pre-rotation
	// password, rather than assuming the write landed before
	// ModifyDBCluster returned.
	var rotated string
	require.Eventuallyf(t, func() bool {
		password, err := resolveManagedMasterPassword(ctx, c.Client, c.secrets, c.ClusterID)
		if err != nil {
			return false
		}
		if password == c.password {
			return false
		}
		rotated = password
		return true
	}, rotationDeadline, rotationPoll,
		"managed rotation did not produce a new password within the deadline")

	// The rotation must land on the real database, not just the control
	// plane's metadata: poll until the new password authenticates.
	rotatedURL := c.URLWithPassword(rotated)
	require.Eventuallyf(t, func() bool {
		conn, err := pgx.Connect(ctx, rotatedURL)
		if err != nil {
			return false
		}
		if err := conn.Close(ctx); err != nil {
			t.Logf("close rotation probe connection: %v", err)
		}
		return true
	}, rotationDeadline, rotationPoll,
		"rotated master password did not become usable within the deadline")

	c.password = rotated
}

// ProvisionAuroraPostgres starts a Ministack container, provisions an
// aurora-postgresql cluster and instance through the real RDS control-plane
// API, waits until the instance is available, and returns the cluster
// handle. The PostgreSQL major follows PG_VERSION: the cluster's database
// is a real postgres container of that major.
func ProvisionAuroraPostgres(t *testing.T) *AuroraCluster {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs Docker")
	}

	ctx := t.Context()
	// No MINISTACK_RDS_PUBLIC_ENDPOINT: in public-endpoint mode a
	// containerized Ministack probes instance readiness at its own
	// loopback, where the host-published sibling port does not exist, so
	// the instance never becomes available. In the default mode the
	// sibling database joins Ministack's Docker network, readiness probes
	// its container IP, and the discovered endpoint is that
	// container-internal address.
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        ministackImage(),
			ExposedPorts: []string{fmt.Sprintf("%d/tcp", ministackGatewayPort)},
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

	clnt, secrets := awsClients(t, ctr)
	major, err := strconv.Atoi(PGVersion())
	require.NoError(t, err, "PG_VERSION must be a PostgreSQL major number")

	const clusterID = "pgsprite-test"
	_, err = clnt.CreateDBCluster(ctx, &rds.CreateDBClusterInput{
		DBClusterIdentifier:      aws.String(clusterID),
		Engine:                   aws.String("aurora-postgresql"),
		EngineVersion:            aws.String(auroraEngineVersion(major)),
		DatabaseName:             aws.String(fixtureDatabase),
		MasterUsername:           aws.String(fixtureUser),
		ManageMasterUserPassword: aws.Bool(true),
	})
	require.NoError(t, err, "create aurora-postgresql cluster")
	password := managedMasterPassword(t, clnt, secrets, clusterID)
	// The database backing the cluster is a sibling Docker container, not a
	// child of the Ministack container — terminating Ministack alone would
	// leak it. Deleting the cluster through the API reaps it. Cleanups run
	// last-in-first-out, so the instance delete registered below runs
	// before this — matching the RDS rule that a cluster cannot be deleted
	// while it still has instances.
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := clnt.DeleteDBCluster(cleanupCtx, &rds.DeleteDBClusterInput{
			DBClusterIdentifier: aws.String(clusterID),
			SkipFinalSnapshot:   aws.Bool(true),
		}); err != nil {
			t.Logf("delete cluster %s: %v", clusterID, err)
		}
	})

	instanceID := clusterID + "-1"
	_, err = clnt.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(instanceID),
		DBClusterIdentifier:  aws.String(clusterID),
		Engine:               aws.String("aurora-postgresql"),
		DBInstanceClass:      aws.String("db.t3.medium"),
	})
	require.NoError(t, err, "create aurora-postgresql instance")
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := clnt.DeleteDBInstance(cleanupCtx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(instanceID),
			SkipFinalSnapshot:    aws.Bool(true),
		}); err != nil {
			t.Logf("delete instance %s: %v", instanceID, err)
		}
	})

	require.Eventuallyf(t, func() bool {
		out, err := clnt.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(instanceID),
		})
		if err != nil || len(out.DBInstances) == 0 {
			return false
		}
		return aws.ToString(out.DBInstances[0].DBInstanceStatus) == rdsStatusAvailable
	}, auroraProvisionDeadline, auroraProvisionPoll,
		"instance %s did not become %s within the provision deadline", instanceID, rdsStatusAvailable)

	// Read the endpoint back from the control plane rather than trusting the
	// request: the discovery flow is the behavior under test, and the
	// address it returns is the address the test connects to. The one
	// exception is a host that cannot route to container IPs (macOS, where
	// Docker runs in a VM) — there the connection falls back to the
	// sibling's host-published port, and only the reachability of the
	// discovered address goes unproven locally; CI runs on Linux, where the
	// discovered endpoint is used directly.
	clusters, err := clnt.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(clusterID),
	})
	require.NoError(t, err, "describe cluster after provisioning")
	require.Len(t, clusters.DBClusters, 1, "provisioned cluster must be discoverable")
	endpoint := aws.ToString(clusters.DBClusters[0].Endpoint)
	port := aws.ToInt32(clusters.DBClusters[0].Port)
	require.NotEmpty(t, endpoint, "cluster endpoint address must be discoverable")
	require.NotZero(t, port, "cluster endpoint port must be discoverable")

	addr := net.JoinHostPort(endpoint, strconv.Itoa(int(port)))
	if !tcpReachable(t, addr) {
		addr = siblingHostAddr(t, ctr, clusterID)
	}

	return &AuroraCluster{
		Client:     clnt,
		ClusterID:  clusterID,
		InstanceID: instanceID,
		addr:       addr,
		password:   password,
		secrets:    secrets,
	}
}

// tcpReachable reports whether addr accepts a TCP connection within
// endpointDialTimeout.
func tcpReachable(t *testing.T, addr string) bool {
	t.Helper()
	dialer := net.Dialer{Timeout: endpointDialTimeout}
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return false
	}
	if err := conn.Close(); err != nil {
		t.Logf("close reachability probe to %s: %v", addr, err)
	}
	return true
}

// siblingHostAddr resolves the host-published address of the sibling
// database container backing the cluster. Ministack publishes the
// sibling's PostgreSQL port on the Docker host, so a host that cannot
// route to container IPs connects through that mapping. The container
// name — "ministack-rds-<sha1(account:region)[:12]>-cluster-<cluster ID>"
// — is an emulator implementation detail this fallback accepts coupling
// to; it is exercised only on hosts where the discovered endpoint is
// unreachable.
func siblingHostAddr(t *testing.T, ctr testcontainers.Container, clusterID string) string {
	t.Helper()
	ctx := t.Context()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	require.NoError(t, err, "create Docker client")
	defer func() {
		if err := docker.Close(); err != nil {
			t.Logf("close Docker client: %v", err)
		}
	}()

	scope := sha1.Sum([]byte(awsAccountID + ":" + awsRegion))
	name := fmt.Sprintf("ministack-rds-%s-cluster-%s", hex.EncodeToString(scope[:])[:12], clusterID)
	inspect, err := docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	require.NoErrorf(t, err, "inspect sibling database container %s", name)

	dbPort, err := network.ParsePort(siblingDBPort)
	require.NoError(t, err, "parse sibling database port")
	bindings := inspect.Container.NetworkSettings.Ports[dbPort]
	require.NotEmptyf(t, bindings, "sibling container %s must publish %s on the host", name, siblingDBPort)

	host, err := ctr.Host(ctx)
	require.NoError(t, err, "resolve Docker host address")
	return net.JoinHostPort(host, bindings[0].HostPort)
}

// awsClients returns RDS and Secrets Manager clients pointed at the
// container's gateway with the emulator's conventional static credentials.
func awsClients(t *testing.T, ctr testcontainers.Container) (*rds.Client, *secretsmanager.Client) {
	t.Helper()
	ctx := t.Context()
	host, err := ctr.Host(ctx)
	require.NoError(t, err, "resolve container host")
	gateway, err := ctr.MappedPort(ctx, fmt.Sprintf("%d/tcp", ministackGatewayPort))
	require.NoError(t, err, "resolve mapped gateway port")
	endpoint := fmt.Sprintf("http://%s:%d", host, gateway.Num())

	// The config is constructed directly, not via config.LoadDefaultConfig:
	// the default loader reads ~/.aws/config, AWS_PROFILE, and the EC2
	// instance-metadata endpoint, so a developer's real AWS environment
	// would leak into a test that must stay hermetic — and it drags the
	// whole credential-discovery chain into go.mod for a client that only
	// ever uses static fixture credentials against the emulator.
	cfg := aws.Config{
		Region:      awsRegion,
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}
	rdsClient := rds.NewFromConfig(cfg, func(o *rds.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	secretsClient := secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	return rdsClient, secretsClient
}

// managedMasterPassword resolves the cluster's RDS-managed master password,
// failing the test on any resolution error.
func managedMasterPassword(t *testing.T, rdsClient *rds.Client, secretsClient *secretsmanager.Client, clusterID string) string {
	t.Helper()
	password, err := resolveManagedMasterPassword(t.Context(), rdsClient, secretsClient, clusterID)
	require.NoError(t, err, "resolve managed master password")
	return password
}

// resolveManagedMasterPassword discovers the cluster's managed master-user
// secret through the RDS control plane and decodes the credentials it holds
// in Secrets Manager, verifying the secret belongs to the fixture master
// user. It returns errors rather than asserting so pollers can retry it.
func resolveManagedMasterPassword(ctx context.Context, rdsClient *rds.Client, secretsClient *secretsmanager.Client, clusterID string) (string, error) {
	clusters, err := rdsClient.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(clusterID),
	})
	if err != nil {
		return "", fmt.Errorf("describe cluster %s: %w", clusterID, err)
	}
	if len(clusters.DBClusters) != 1 {
		return "", fmt.Errorf("describe cluster %s: expected one cluster, got %d", clusterID, len(clusters.DBClusters))
	}
	secretMeta := clusters.DBClusters[0].MasterUserSecret
	if secretMeta == nil || aws.ToString(secretMeta.SecretArn) == "" {
		return "", fmt.Errorf("cluster %s exposes no managed master-user secret ARN", clusterID)
	}
	secret, err := secretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: secretMeta.SecretArn,
	})
	if err != nil {
		return "", fmt.Errorf("get managed master-user secret for cluster %s: %w", clusterID, err)
	}
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(secret.SecretString)), &creds); err != nil {
		return "", fmt.Errorf("decode managed master-user secret for cluster %s: %w", clusterID, err)
	}
	if creds.Username != fixtureUser {
		return "", fmt.Errorf("managed secret username %q does not match master user %q", creds.Username, fixtureUser)
	}
	if creds.Password == "" {
		return "", fmt.Errorf("managed secret for cluster %s holds an empty password", clusterID)
	}
	return creds.Password, nil
}
