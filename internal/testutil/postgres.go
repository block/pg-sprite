// Package testutil is the integration-test harness: a real PostgreSQL in a
// container plus per-test throwaway schemas. Integration tests are the
// workhorse of this repo — core logic is validated against a real database,
// not mocks.
package testutil

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

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

// StartPostgres starts a disposable PostgreSQL container for the test and
// returns its connection URL. The container is terminated when the test ends.
// Set SKIP_INTEGRATION=1 to skip tests that need Docker.
func StartPostgres(t *testing.T) string {
	t.Helper()
	if os.Getenv("SKIP_INTEGRATION") != "" {
		t.Skip("SKIP_INTEGRATION set; skipping test that needs Docker")
	}
	// The container must outlive t.Context (which is cancelled before
	// cleanups run), so use Background and terminate via t.Cleanup.
	ctx := context.Background()
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

// NewSchema creates a unique throwaway schema on pool, sets it up for
// cleanup, and returns its name. Tests qualify their objects with it so
// parallel tests on one container never collide.
func NewSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	name := fmt.Sprintf("t_%d_%d", os.Getpid(), schemaSeq.Add(1))
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE SCHEMA %s", name))
	require.NoError(t, err, "create throwaway schema")
	t.Cleanup(func() {
		// t.Context is done by cleanup time; use a fresh context.
		_, err := pool.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", name))
		if err != nil {
			t.Logf("drop throwaway schema %s: %v", name, err)
		}
	})
	return name
}
