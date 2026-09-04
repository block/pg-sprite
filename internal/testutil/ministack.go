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
	"net/url"
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
	"github.com/stretchr/testify/assert"
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
	// rotationDeadline bounds each phase of the managed-password flow
	// separately: how long a written secret may take to become resolvable
	// through the control plane, and how long a rotated password may take
	// to land on the running database. A rotation that exhausts both
	// budgets therefore takes up to twice this value.
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
	// Ministack scopes each database container's name by
	// sha1(account:region), so these also feed memberHostAddr.
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
	return "ministackorg/ministack:1.5.6-full"
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
	// ReaderInstanceID is the cluster member backed by a real PostgreSQL
	// hot standby when the harness enables Ministack's replication mode.
	ReaderInstanceID string

	// addr is the host:port the test connects to — the discovered cluster
	// endpoint when reachable, otherwise the sibling container's
	// host-published address (see ProvisionAuroraPostgres).
	addr       string
	readerAddr string
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
	return c.urlWithPassword(c.password)
}

// ReaderURL returns a connection URL for the cluster's read-only standby.
func (c *AuroraCluster) ReaderURL() string {
	return c.urlAt(c.readerAddr, c.password)
}

// urlWithPassword returns a connection URL using the given master
// password. The password is RDS-generated — the harness does not choose
// it — so it may contain URL-reserved characters; the URL is assembled
// with net/url, which escapes each component, never by string
// interpolation.
//
// sslmode=disable: the sibling database container runs plain PostgreSQL
// without TLS, and the endpoint is not an *.rds.amazonaws.com hostname,
// so the production TLS path is out of scope for this tier (it is
// proven by pkg/dbconn's TLS integration tests).
func (c *AuroraCluster) urlWithPassword(password string) string {
	return c.urlAt(c.addr, password)
}

func (c *AuroraCluster) urlAt(addr, password string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(fixtureUser, password),
		Host:     addr,
		Path:     "/" + fixtureDatabase,
		RawQuery: "sslmode=disable",
	}
	return u.String()
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

	rotated := awaitManagedMasterPassword(t, c.Client, c.secrets, c.ClusterID, c.password)

	// The rotation must land on the real database, not just the control
	// plane's metadata: poll until the new password authenticates.
	rotatedURL := c.urlWithPassword(rotated)
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
			Env: map[string]string{
				"MINISTACK_RDS_PG_CLUSTER_REPLICATION": "1",
			},
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
	// The database backing the cluster is a sibling Docker container, not a
	// child of the Ministack container — terminating Ministack alone would
	// leak it. Deleting the cluster through the API reaps it. Cleanups run
	// last-in-first-out, so the instance delete registered below runs
	// before this — matching the RDS rule that a cluster cannot be deleted
	// while it still has instances. Registered before anything fallible
	// touches the cluster, so a failure later in provisioning cannot leak
	// the sibling container.
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := clnt.DeleteDBCluster(cleanupCtx, &rds.DeleteDBClusterInput{
			DBClusterIdentifier: aws.String(clusterID),
			SkipFinalSnapshot:   aws.Bool(true),
		}); err != nil {
			t.Logf("delete cluster %s: %v", clusterID, err)
		}
	})
	password := awaitManagedMasterPassword(t, clnt, secrets, clusterID, "")

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

	readerInstanceID := clusterID + "-reader"
	_, err = clnt.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(readerInstanceID),
		DBClusterIdentifier:  aws.String(clusterID),
		Engine:               aws.String("aurora-postgresql"),
		DBInstanceClass:      aws.String("db.t3.medium"),
	})
	require.NoError(t, err, "create aurora-postgresql reader instance")
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if _, err := clnt.DeleteDBInstance(cleanupCtx, &rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(readerInstanceID),
			SkipFinalSnapshot:    aws.Bool(true),
		}); err != nil {
			t.Logf("delete reader instance %s: %v", readerInstanceID, err)
		}
	})

	var readerEndpoint string
	var readerPort int32
	require.Eventuallyf(t, func() bool {
		out, err := clnt.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(readerInstanceID),
		})
		if err != nil || len(out.DBInstances) == 0 ||
			aws.ToString(out.DBInstances[0].DBInstanceStatus) != rdsStatusAvailable {
			return false
		}
		readerEndpoint = aws.ToString(out.DBInstances[0].Endpoint.Address)
		readerPort = aws.ToInt32(out.DBInstances[0].Endpoint.Port)
		return readerEndpoint != "" && readerPort != 0
	}, auroraProvisionDeadline, auroraProvisionPoll,
		"reader instance %s did not become %s within the provision deadline", readerInstanceID, rdsStatusAvailable)

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
		addr = memberHostAddr(t, ctr, memberKindCluster, clusterID)
	}
	readerAddr := net.JoinHostPort(readerEndpoint, strconv.Itoa(int(readerPort)))
	if !tcpReachable(t, readerAddr) {
		readerAddr = memberHostAddr(t, ctr, memberKindInstance, readerInstanceID)
	}

	return &AuroraCluster{
		Client:           clnt,
		ClusterID:        clusterID,
		InstanceID:       instanceID,
		ReaderInstanceID: readerInstanceID,
		addr:             addr,
		readerAddr:       readerAddr,
		password:         password,
		secrets:          secrets,
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

// memberKind is the segment of a Ministack database-container name that
// says which identifier keys it: the cluster's shared writer container is
// named after the cluster, a replicating reader after its instance.
type memberKind string

const (
	memberKindCluster  memberKind = "cluster"
	memberKindInstance memberKind = "instance"
)

// memberHostAddr resolves the host-published address of the database
// container backing a cluster member. Ministack publishes each member's
// PostgreSQL port on the Docker host, so a host that cannot route to
// container IPs connects through that mapping. The container name —
// "ministack-rds-<sha1(account:region)[:12]>-<kind>-<identifier>" — is an
// emulator implementation detail this fallback accepts coupling to; it is
// exercised only on hosts where the discovered endpoint is unreachable.
func memberHostAddr(t *testing.T, ctr testcontainers.Container, kind memberKind, identifier string) string {
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
	name := fmt.Sprintf("ministack-rds-%s-%s-%s", hex.EncodeToString(scope[:])[:12], kind, identifier)
	inspect, err := docker.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	require.NoErrorf(t, err, "inspect %s database container %s", kind, name)

	dbPort, err := network.ParsePort(siblingDBPort)
	require.NoError(t, err, "parse member database port")
	bindings := inspect.Container.NetworkSettings.Ports[dbPort]
	require.NotEmptyf(t, bindings, "%s container %s must publish %s on the host", kind, name, siblingDBPort)

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

// awaitManagedMasterPassword polls until the cluster's RDS-managed master
// password resolves through the control plane and differs from previous,
// then returns it. The secret write is the control plane's to sequence —
// after CreateDBCluster and after a rotation alike, the caller cannot
// assume the write landed before the API call returned, so a transient
// resolution failure is retried rather than failing the test. Pass
// previous "" during provisioning, when any resolved password is
// acceptable. A deadline failure reports the last resolution error.
func awaitManagedMasterPassword(t *testing.T, rdsClient *rds.Client, secretsClient *secretsmanager.Client, clusterID, previous string) string {
	t.Helper()
	var resolved string
	require.EventuallyWithTf(t, func(collect *assert.CollectT) {
		password, err := resolveManagedMasterPassword(t.Context(), rdsClient, secretsClient, clusterID)
		if !assert.NoErrorf(collect, err, "resolve managed master password for cluster %s", clusterID) {
			return
		}
		if !assert.NotEqual(collect, previous, password,
			"managed secret still resolves to the previous password") {
			return
		}
		resolved = password
	}, rotationDeadline, rotationPoll,
		"managed master password for cluster %s did not become resolvable within the deadline", clusterID)
	return resolved
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
