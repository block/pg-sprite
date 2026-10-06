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
	"strings"
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

// StartPostgresWithSettings returns a connection URL for a dedicated
// PostgreSQL container started with the given postgresql.conf settings
// (each "name=value"), for tests that control a restart-only setting such
// as wal_level or max_replication_slots. Unlike StartPostgres it never uses
// PG_DSN — a shared server cannot change those settings — and so costs a
// container start of its own. Set SKIP_INTEGRATION=1 to skip.
func StartPostgresWithSettings(t *testing.T, settings ...string) string {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs a database")
	}
	args := make([]string, 0, 2*len(settings))
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	// t.Context only governs the start request; the running container is
	// not tied to it and is terminated via t.Cleanup below.
	ctx := t.Context()
	ctr, err := tcpostgres.Run(ctx, "postgres:"+PGVersion(),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithCmdArgs(args...),
	)
	require.NoError(t, err, "start postgres container with settings %v", settings)
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
	return newDatabase(t, serverURL, "")
}

// NewDatabaseWithEncoding is NewDatabase for a database created with the
// given server encoding (e.g. "SQL_ASCII") under the C locale, for tests
// whose subject is how the engine treats the bytes a database stores.
// template0 is the template, as a server encoding other than the
// cluster's requires.
func NewDatabaseWithEncoding(t *testing.T, serverURL, encoding string) string {
	t.Helper()
	return newDatabase(t, serverURL, " ENCODING "+quoteLiteral(encoding)+" LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0")
}

// NewDatabaseFromTemplate is NewDatabase for a database cloned from the
// one templateURL connects to, for tests whose subject is what a clone
// shares with its template: the relations keep their OIDs. The server must
// hold no other session on the template while it is copied, so the caller
// closes every pool on it first.
func NewDatabaseFromTemplate(t *testing.T, serverURL, templateURL string) string {
	t.Helper()
	u, err := url.Parse(templateURL)
	require.NoError(t, err, "parse template URL")
	template := strings.TrimPrefix(u.Path, "/")
	require.NotEmpty(t, template, "template URL names no database")
	return newDatabase(t, serverURL, " TEMPLATE "+pgx.Identifier{template}.Sanitize())
}

func newDatabase(t *testing.T, serverURL, createOptions string) string {
	t.Helper()
	name := fmt.Sprintf("db_%d_%d", os.Getpid(), schemaSeq.Add(1))

	pool, err := pgxpool.New(t.Context(), serverURL)
	require.NoError(t, err, "connect to create throwaway database")
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+createOptions)
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

// NewCatalogShadowingPool returns a pool whose every session has schema
// ahead of pg_catalog on search_path, so impostor relations, views,
// functions, and operators created in schema answer unqualified catalog
// names. It is a raw pgx pool on purpose: pools from pkg/dbconn remove a
// shadowed pg_catalog entry on connect, which would make the impostors
// unreachable, and the tests that use this pool prove that the queries
// themselves stay pg_catalog-qualified for a pool the library caller built.
// It carries none of pkg/dbconn's session defaults.
func NewCatalogShadowingPool(t *testing.T, serverURL, schema string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err, "parse server URL")
	pc.ConnConfig.RuntimeParams["search_path"] = schema + ", pg_catalog"
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err, "connect with a shadowing search_path")
	t.Cleanup(pool.Close)
	var path string
	require.NoError(t, pool.QueryRow(t.Context(), "SHOW search_path").Scan(&path))
	require.Equal(t, schema+", pg_catalog", path, "the shadowing search_path must survive connect")
	return pool
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

// NewRole creates a throwaway cluster-level role with the given options and
// registers its drop. Roles are cluster-scoped, so names are unique per
// process the same way throwaway schemas are.
func NewRole(t *testing.T, pool *pgxpool.Pool, options string) string {
	t.Helper()
	name := fmt.Sprintf("r_%d_%d", os.Getpid(), schemaSeq.Add(1))
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE ROLE %s %s", pgx.Identifier{name}.Sanitize(), options))
	require.NoError(t, err, "create throwaway role")
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation.
		_, err := pool.Exec(context.WithoutCancel(t.Context()),
			"DROP ROLE IF EXISTS "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Logf("drop throwaway role %s: %v", name, err)
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
