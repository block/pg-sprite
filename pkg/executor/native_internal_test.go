// White-box tests for the fail-closed decision helpers of the concurrent
// index build: the pieces whose safety branches (unknown backend states, a
// replaced target table) cannot be reached deterministically through the
// public API against a healthy database.

package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

func TestClassifyBackendState(t *testing.T) {
	tests := []struct {
		name  string
		state *string
		want  backendVerdict
	}{
		{name: "idle is stopped", state: new("idle"), want: backendStopped},
		{name: "idle in transaction is stopped", state: new("idle in transaction"), want: backendStopped},
		{name: "idle in aborted transaction is stopped", state: new("idle in transaction (aborted)"), want: backendStopped},
		{name: "active keeps polling", state: new("active"), want: backendRunning},
		{name: "starting keeps polling", state: new("starting"), want: backendRunning},
		{name: "fastpath function call keeps polling", state: new("fastpath function call"), want: backendRunning},
		{name: "NULL state is unprovable: the backend is hidden", state: nil, want: backendUnprovable},
		{name: "disabled is unprovable: track_activities is off", state: new("disabled"), want: backendUnprovable},
		{name: "an unknown state is unprovable", state: new("hibernating"), want: backendUnprovable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyBackendState(tt.state))
		})
	}
}

// TestFailedBuildVerdictFailsClosedOnChangedTargetIdentity covers the
// verdict's identity guard: the pinned table OID no longer resolving —
// because the table was replaced or dropped outright while the build ran —
// must yield an indeterminate, fail-closed *InvalidIndexError carrying the
// original build failure, never a clean pass-through.
func TestFailedBuildVerdictFailsClosedOnChangedTargetIdentity(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	build := concurrentIndexBuild{index: "idx_c", tableSchema: schema, table: "t"}
	buildErr := errors.New("the build failure under verdict")
	// A PID no backend owns: pg_stat_activity has no row for it, which the
	// wait correctly reads as a disconnected — provably stopped — backend.
	const stoppedPID = 0

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	pinned, err := resolveTarget(t.Context(), pool, build)
	require.NoError(t, err)

	t.Run("replaced table fails closed", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(
			"DROP TABLE %s.t; CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema, schema))
		require.NoError(t, err)

		verdict := failedBuildVerdict(t.Context(), pool, build, pinned, stoppedPID, buildErr)

		require.ErrorIs(t, verdict, ErrTargetIdentityChanged)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, verdict, &invalidErr)
		assert.Equal(t, schema, invalidErr.Schema)
		assert.Equal(t, "idx_c", invalidErr.Index)
		assert.ErrorIs(t, invalidErr.Build, buildErr, "the original build failure must ride inside the typed outcome")
	})

	t.Run("dropped table fails closed", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP TABLE %s.t", schema))
		require.NoError(t, err)

		verdict := failedBuildVerdict(t.Context(), pool, build, pinned, stoppedPID, buildErr)

		require.ErrorIs(t, verdict, ErrTargetIdentityChanged)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, verdict, &invalidErr)
		assert.ErrorIs(t, invalidErr.Build, buildErr)
	})
}

// TestCatalogVerdictReportsDebrisOnAnotherTable covers the swap-and-restore
// race: the pinned table name resolves back to the pinned OID, but the
// build's debris landed on a different table that briefly owned the name.
// The schema-wide name inspection must still find it — an OID-pinned check
// would go blind and report clean.
func TestCatalogVerdictReportsDebrisOnAnotherTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	build := concurrentIndexBuild{index: "idx_c", tableSchema: schema, table: "t"}
	buildErr := errors.New("the build failure under verdict")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (id int PRIMARY KEY, c int);
		CREATE TABLE %s.u (id int PRIMARY KEY, c int);
		INSERT INTO %s.u VALUES (1, 7), (2, 7)`, schema, schema, schema))
	require.NoError(t, err)
	pinned, err := resolveTarget(t.Context(), pool, build)
	require.NoError(t, err)
	// The real debris of a failed concurrent build, under the build's
	// requested name but on the other table.
	_, err = pool.Exec(t.Context(),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_c ON %s.u (c)", schema))
	require.Error(t, err, "a unique build over duplicates must fail")

	verdict := catalogVerdict(t.Context(), pool, build, pinned, buildErr)

	require.ErrorIs(t, verdict, ErrBuildLeftInvalidIndex)
	var invalidErr *InvalidIndexError
	require.ErrorAs(t, verdict, &invalidErr)
	assert.ErrorIs(t, invalidErr.Build, buildErr)
}

// TestCatalogVerdictFailsClosedWhenInspectionFails covers the verdict's own
// failure: when the catalog snapshot cannot be read at all, the outcome
// must be the fail-closed recovery report carrying the original build
// failure — never a clean pass-through. The fault is real: the pool is
// closed before the verdict runs.
func TestCatalogVerdictFailsClosedWhenInspectionFails(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)

	build := concurrentIndexBuild{index: "idx_c", tableSchema: schema, table: "t"}
	buildErr := errors.New("the build failure under verdict")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	pinned, err := resolveTarget(t.Context(), pool, build)
	require.NoError(t, err)

	pool.Close()
	verdict := catalogVerdict(t.Context(), pool, build, pinned, buildErr)

	var invalidErr *InvalidIndexError
	require.ErrorAs(t, verdict, &invalidErr)
	assert.ErrorIs(t, invalidErr.Build, buildErr, "the original build failure must ride inside the typed outcome")
	require.NotNil(t, invalidErr.Cleanup, "the inspection failure must be reported as the recovery cause")
	assert.NotErrorIs(t, invalidErr.Cleanup, ErrTargetIdentityChanged, "an unreadable catalog is an inspection failure, not an identity verdict")
	assert.NotErrorIs(t, invalidErr.Cleanup, ErrBuildLeftInvalidIndex, "an unreadable catalog cannot prove a leftover")
}
