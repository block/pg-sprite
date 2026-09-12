// White-box tests for the fail-closed decision helpers of the concurrent
// index build: the pieces whose safety branches (unknown backend states, a
// replaced target table) cannot be reached deterministically through the
// public API against a healthy database.

package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
)

func (c *cancelOnSecondRead) Now() time.Time {
	c.reads++
	if c.reads == 2 {
		c.cancel()
	}
	return time.Now()
}

// TestBuildIndexConcurrentlyVerifiesASuccessfulBuildAfterTheCallerCancels
// pins the success path's detached verdict: a build the server completed
// is evidence the caller is owed, and the caller's context ending a moment
// later must not turn it into an unproven outcome. The verdict runs on its
// own bounded context, so the report is returned and the index is valid.
func TestBuildIndexConcurrentlyVerifiesASuccessfulBuildAfterTheCallerCancels(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	tracker, err := progress.NewTracker(&cancelOnSecondRead{cancel: cancel})
	require.NoError(t, err)

	rep, err := buildIndexConcurrently(ctx, pool, fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema),
		ConcurrentBudget{CallerOwned: true}, tracker)
	require.Error(t, ctx.Err(), "the fixture must have ended the caller's context before the verdict")
	require.NoError(t, err, "a build the server completed is reported, whatever the caller's context did after")
	assert.Equal(t, "idx_c", rep.Index)
	assert.NotZero(t, rep.IndexOID)

	var valid bool
	require.NoError(t, pool.QueryRow(context.WithoutCancel(t.Context()),
		`SELECT i.indisvalid FROM pg_catalog.pg_index i WHERE i.indexrelid OPERATOR(pg_catalog.=) $1`, rep.IndexOID).Scan(&valid))
	assert.True(t, valid)
}

// TestInvalidIndexErrorAdviceMatchesProof is the renderer's own unit test:
// the message may name a DROP INDEX CONCURRENTLY only in the one state
// where the entry is proven this build's own leftover, and may point at
// the automatic recovery only in the states that recovery accepts. Every
// other state must not hand the operator a destructive statement — the
// index under that name may be healthy or another actor's build in
// progress.
func TestVerifiedBuildReportFailsClosed(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	build := concurrentIndexBuild{index: "idx_c", tableSchema: schema, table: "t"}
	target := indexTarget{schema: schema}
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); INSERT INTO %s.t VALUES (1, 7), (2, 7)", schema, schema))
	require.NoError(t, err)
	// The real invalid entry a failed concurrent build leaves behind.
	_, err = pool.Exec(t.Context(),
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema))
	require.Error(t, err, "a unique build over duplicates must fail")

	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)

	t.Run("an invalid entry under the build's name fails closed", func(t *testing.T) {
		_, err := verifiedBuildReport(t.Context(), conn, build, target, time.Second)
		require.ErrorIs(t, err, ErrBuildLeftInvalidIndex)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.Equal(t, schema, invalidErr.Schema)
		assert.Equal(t, "idx_c", invalidErr.Index)
	})

	t.Run("an unreadable catalog fails closed", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := verifiedBuildReport(cancelledCtx, conn, build, target, time.Second)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.NotErrorIs(t, err, ErrBuildLeftInvalidIndex, "an unreadable catalog cannot prove a leftover")
		require.NotNil(t, invalidErr.Cleanup, "the verification failure must be reported as the recovery cause")
	})
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
// would go blind and report clean — and must report the entry where it is,
// on the other table, without claiming it as this build's own leftover.
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

	require.ErrorIs(t, verdict, ErrInvalidIndexOnOtherTable)
	var invalidErr *InvalidIndexError
	require.ErrorAs(t, verdict, &invalidErr)
	assert.Equal(t, "u", invalidErr.Table, "the verdict names the table the debris actually sits on")
	assert.ErrorIs(t, invalidErr.Build, buildErr)
	assert.False(t, invalidErr.Recoverable(), "a foreign table's entry is not this change's to recover")
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

// TestDroppableColumnMatchesTheServer pins the droppability predicate to
// the server's own answer, one index shape at a time: the predicate says
// droppable exactly when DROP INDEX CONCURRENTLY succeeds, and every shape
// it refuses is one the server refuses too, matched by SQLSTATE. The
// constraint term is exercised on a plain table with an index that is
// invalid in no other respect — a foreign key's referenced unique index,
// which no constraint of its own table names — because a constraint's
// index can never be a failed concurrent build's debris and so is not
// reachable through the recovery's public path.
