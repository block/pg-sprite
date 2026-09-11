package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PoolMode is a connection pooler's pooling granularity, which is what
// decides whether a client connection keeps one server session.
type PoolMode string

const (
	// TransactionPooling returns the backend to the pooler's own pool at
	// the end of every transaction, so a client connection has no stable
	// server session. It is the default on hosted PostgreSQL platforms.
	TransactionPooling PoolMode = "transaction"
	// SessionPooling gives a client connection its own backend for the
	// connection's lifetime, which is all a session-scoped lock needs.
	SessionPooling PoolMode = "session"
)

// pgBouncerImage is pinned: the pooling behaviour under test is the whole
// point of the fixture, so the version is not left to a floating tag.
const pgBouncerImage = "edoburu/pgbouncer:v1.25.2-p0"

// pgBouncerPort is the port PgBouncer listens on inside its container.
const pgBouncerPort = "6432"

const (
	// poolerReadyDeadline bounds how long the pooler gets to accept a login
	// that reaches the server behind it.
	poolerReadyDeadline = 30 * time.Second
	poolerReadyPoll     = 100 * time.Millisecond
)

// StartPostgresBehindPgBouncer starts a PostgreSQL server with a PgBouncer
// in front of it in the given pool mode and returns the pooled connection
// URL — the one a hosted platform would hand an operator.
//
// It always starts its own containers: the pooling mode is the fixture, so
// an external PG_DSN cannot substitute for it.
func StartPostgresBehindPgBouncer(t *testing.T, mode PoolMode) (pooledURL string) {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs a database")
	}
	const (
		user     = "postgres"
		password = "postgres"
		database = "postgres"
		alias    = "upstream-postgres"
	)
	ctx := t.Context()

	net, err := tcnetwork.New(ctx)
	require.NoError(t, err, "create the pooler test network")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation, or
		// the removal always fails and each run leaks a bridge network until
		// the address pool is exhausted.
		if err := net.Remove(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("remove the pooler test network: %v", err)
		}
	})

	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:" + PGVersion(),
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     user,
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       database,
			},
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {alias}},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2),
		},
		Started: true,
	})
	require.NoError(t, err, "start the upstream postgres container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(server); err != nil {
			t.Logf("terminate the upstream postgres container: %v", err)
		}
	})

	pooler, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        pgBouncerImage,
			ExposedPorts: []string{pgBouncerPort + "/tcp"},
			Env: map[string]string{
				"DB_HOST":     alias,
				"DB_PORT":     "5432",
				"DB_USER":     user,
				"DB_PASSWORD": password,
				"DB_NAME":     database,
				"POOL_MODE":   string(mode),
				"LISTEN_PORT": pgBouncerPort,
				"AUTH_TYPE":   "scram-sha-256",
				// Transaction pooling breaks prepared statements unless the
				// pooler tracks them itself, and pgx prepares by default.
				// Tracking them keeps the fixture about session binding
				// rather than about protocol support.
				"MAX_PREPARED_STATEMENTS": "100",
				// More than one server connection, so the pooler has a
				// backend to rebind to rather than trivially reusing one.
				"DEFAULT_POOL_SIZE": "5",
				"MAX_CLIENT_CONN":   "50",
				// PgBouncer rejects a startup parameter it does not know how
				// to track, and pg-sprite sends its session timeouts that
				// way. A deployment behind a pooler configures these for the
				// same reason, so the fixture does too.
				"IGNORE_STARTUP_PARAMETERS": "lock_timeout,statement_timeout,extra_float_digits",
			},
			Networks: []string{net.Name},
			// A bound port only says the process is up. The host side of a
			// published port accepts a client before the container does, so
			// readiness is a login through the pooler, proven below.
			WaitingFor: wait.ForListeningPort(pgBouncerPort + "/tcp"),
		},
		Started: true,
	})
	require.NoError(t, err, "start the pgbouncer container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(pooler); err != nil {
			t.Logf("terminate the pgbouncer container: %v", err)
		}
	})

	pooledURL = containerURL(t, pooler, pgBouncerPort, user, password, database)
	awaitPooledLogin(t, pooledURL)
	return pooledURL
}

// awaitPooledLogin polls until a client can log in through the pooler and
// run a statement on the server behind it — the property the returned URL
// promises. A client that arrives earlier is accepted by the port forwarder
// and then cut off on its first read, which looks like a broken pooler
// rather than an early caller.
func awaitPooledLogin(t *testing.T, pooledURL string) {
	t.Helper()
	require.EventuallyWithTf(t, func(collect *assert.CollectT) {
		ctx := t.Context()
		conn, err := pgx.Connect(ctx, pooledURL)
		if !assert.NoError(collect, err, "log in through the pooler") {
			return
		}
		defer func() {
			if err := conn.Close(ctx); err != nil {
				t.Logf("close the pooler readiness probe connection: %v", err)
			}
		}()
		var one int
		assert.NoError(collect, conn.QueryRow(ctx, "SELECT 1").Scan(&one), "run a statement through the pooler")
	}, poolerReadyDeadline, poolerReadyPoll,
		"the pooler did not accept a login that reached the server within %s", poolerReadyDeadline)
}

// containerURL builds a connection URL for a container's mapped port.
func containerURL(t *testing.T, ctr testcontainers.Container, port, user, password, database string) string {
	t.Helper()
	host, err := ctr.Host(t.Context())
	require.NoError(t, err, "container host")
	mapped, err := ctr.MappedPort(t.Context(), port+"/tcp")
	require.NoError(t, err, "container mapped port %s", port)
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, password, host, mapped.Port(), database)
}
