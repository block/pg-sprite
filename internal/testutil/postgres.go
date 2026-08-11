// Package testutil is the integration-test harness: a real PostgreSQL in a
// container plus per-test throwaway schemas. Integration tests are the
// workhorse of this repo — core logic is validated against a real database,
// not mocks.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// DefaultPGVersion is the major used when PG_VERSION is unset. CI overrides
// it across the full supported matrix (14 → 18).
const DefaultPGVersion = "16"

// PGVersion returns the PostgreSQL major version under test.
func PGVersion() string {
	if v := os.Getenv("PG_VERSION"); v != "" {
		return v
	}
	return DefaultPGVersion
}

// StartPostgres returns a PostgreSQL connection URL for the test.
//
// By default it starts a disposable container (terminated when the test
// ends). When PG_DSN is set, that external server is used instead and no
// container is started — the compose/ workflow and CI variants that run a
// long-lived server use this. Set SKIP_INTEGRATION=1 to skip tests that need
// a database entirely.
func StartPostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs a database")
	}
	if dsn := os.Getenv("PG_DSN"); dsn != "" {
		return dsn
	}
	// t.Context only governs the start request; the running container is
	// not tied to it and is terminated via t.Cleanup below.
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, "postgres:"+PGVersion(), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "container connection string")
	return url
}

var schemaSeq atomic.Int64

// NewDatabase creates a unique throwaway database on the server at serverURL,
// sets it up for cleanup, and returns a URL that connects to it. Throwaway
// schemas do not isolate pg_stat_activity, so a test that must observe an
// exact session set (e.g. none) on a shared server gets a database of its
// own. serverURL must be in URL form (postgres://...), which StartPostgres
// always returns.
func NewDatabase(t *testing.T, serverURL string) string {
	t.Helper()
	name := fmt.Sprintf("db_%d_%d", os.Getpid(), schemaSeq.Add(1))

	pool, err := pgxpool.New(t.Context(), serverURL)
	require.NoError(t, err, "connect to create throwaway database")
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err, "create throwaway database")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		// FORCE terminates any connection a test leaked into the database.
		ctx := context.WithoutCancel(t.Context())
		_, err := pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			t.Logf("drop throwaway database %s: %v", name, err)
		}
	})

	u, err := url.Parse(serverURL)
	require.NoError(t, err, "parse server URL")
	require.NotEmpty(t, u.Scheme, "NewDatabase needs a URL-form DSN (postgres://...)")
	u.Path = "/" + name
	return u.String()
}

// NewSchema creates a unique throwaway schema on pool, sets it up for
// cleanup, and returns its name. Tests qualify their objects with it so
// parallel tests on one container never collide.
func NewSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("t_%d_%d", os.Getpid(), schemaSeq.Add(1))
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE SCHEMA %s", name))
	require.NoError(t, err, "create throwaway schema")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", name))
		if err != nil {
			t.Logf("drop throwaway schema %s: %v", name, err)
		}
	})
	return name
}

// NewPublicTable creates a uniquely named throwaway table in the public
// schema — for tests that exercise unqualified-statement resolution, where
// a dedicated schema would defeat the point — and returns its name. The
// unique name keeps a shared PG_DSN database safe; cleanup drops the table.
func NewPublicTable(t *testing.T, pool *pgxpool.Pool, columns string) string {
	t.Helper()
	name := fmt.Sprintf("t_%d_%d", os.Getpid(), schemaSeq.Add(1))
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE public.%s %s", name, columns))
	require.NoError(t, err, "create throwaway public table")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf("DROP TABLE IF EXISTS public.%s", name))
		if err != nil {
			t.Logf("drop throwaway public table %s: %v", name, err)
		}
	})
	return name
}
