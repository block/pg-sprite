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

	"github.com/jackc/pgx/v5/pgconn"
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

// TestAsConcurrentBudgetError covers the 57014 disambiguation: SQLSTATE
// 57014 is query_canceled generally, so only a cancellation arriving at or
// after the overall budget can be the budget's own statement_timeout; an
// earlier one is an external cancellation and must not read as budget
// exhaustion — a consumer branching on *BudgetError escalates to a heavier
// strategy, which is the wrong reaction to a deliberate operator cancel.
func TestAsConcurrentBudgetError(t *testing.T) {
	budget := ConcurrentBudget{Overall: time.Minute}
	cancelled := &pgconn.PgError{Code: sqlstateQueryCanceled}

	t.Run("57014 at or past the budget is budget exhaustion", func(t *testing.T) {
		err := asConcurrentBudgetError(cancelled, budget, time.Minute+time.Second, "concurrent index build")
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
	})

	t.Run("57014 before the budget is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(cancelled, budget, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "an external cancel must not read as budget exhaustion")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable for SQLSTATE branching")
		assert.Equal(t, sqlstateQueryCanceled, pgErr.Code)
	})

	t.Run("57014 under a caller-owned deadline is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(cancelled, ConcurrentBudget{CallerOwned: true}, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})

	t.Run("any other failure is neither", func(t *testing.T) {
		err := asConcurrentBudgetError(&pgconn.PgError{Code: "42P07"}, budget, 2*time.Second, "concurrent index build")
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})
}

// TestInvalidIndexErrorAdviceMatchesProof is the renderer's own unit test:
// the message may name a DROP INDEX CONCURRENTLY only in the one state
// where the entry is proven this build's own leftover, and may point at
// the automatic recovery only in the states that recovery accepts. Every
// other state must not hand the operator a destructive statement — the
// index under that name may be healthy or another actor's build in
// progress.
func TestInvalidIndexErrorAdviceMatchesProof(t *testing.T) {
	tests := []struct {
		name          string
		cleanup       error
		wantsDrop     bool
		wantsRecovery bool
	}{
		{name: "proven own leftover names the drop", cleanup: ErrBuildLeftInvalidIndex, wantsDrop: true, wantsRecovery: true},
		{name: "abandoned entry names the recovery only", cleanup: ErrAbandonedInvalidIndex, wantsRecovery: true},
		{name: "in-flight build does not", cleanup: ErrInvalidIndexBuildInFlight},
		{name: "another table's entry does not", cleanup: ErrInvalidIndexOnOtherTable},
		{name: "unobservable builder names the recovery only", cleanup: ErrInvalidIndexBuilderUnobservable, wantsRecovery: true},
		{name: "changed target identity does not", cleanup: ErrTargetIdentityChanged},
		{name: "unproven abandonment does not", cleanup: ErrAbandonmentUnproven},
		{name: "an inspection failure does not", cleanup: errors.New("inspect index s.i: closed pool")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &InvalidIndexError{Schema: "s", Index: "i", Table: "t", BuilderPID: 7, Cleanup: tt.cleanup}
			msg := e.Error()
			if tt.wantsDrop {
				assert.Contains(t, msg, `DROP INDEX CONCURRENTLY "s"."i"`)
			} else {
				assert.NotContains(t, msg, "DROP INDEX")
			}
			if tt.wantsRecovery {
				assert.Contains(t, msg, "RebuildAbandonedIndex")
			} else {
				assert.NotContains(t, msg, "RebuildAbandonedIndex")
			}
			assert.Equal(t, tt.wantsRecovery, e.Recoverable(), "the advice and Recoverable must agree")
		})
	}
}

// TestClassifyInvalidIndexOrdersByProofStrength pins the classifier's
// order: a visible builder outranks the table check, the table check
// outranks visibility, and only a fully observable silence on the target
// table is called abandoned.
func TestClassifyInvalidIndexOrdersByProofStrength(t *testing.T) {
	target := indexTarget{tableOID: 100, schema: "s"}
	observable := builderFacts{tracking: true}
	tests := []struct {
		name     string
		existing invalidIndex
		want     error
		wantPID  uint32
	}{
		{
			name:     "visible builder on the target table is in flight",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", builder: builderFacts{pid: 42, tracking: true}},
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "visible builder on another table is still in flight",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", builder: builderFacts{pid: 42, hiddenRows: 3}},
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "another table's entry is refused before visibility is consulted",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", builder: builderFacts{hiddenRows: 1}},
			want:     ErrInvalidIndexOnOtherTable,
		},
		{
			name:     "a hidden command makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", builder: builderFacts{hiddenRows: 1, tracking: true}},
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "activity tracking off makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", builder: builderFacts{tracking: false}},
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "observable silence on the target table is abandoned",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", builder: observable},
			want:     ErrAbandonedInvalidIndex,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := classifyInvalidIndex(tt.existing, target, "i")
			require.ErrorIs(t, e, tt.want)
			assert.Equal(t, "s", e.Schema)
			assert.Equal(t, "i", e.Index)
			assert.Equal(t, tt.existing.table, e.Table)
			assert.Equal(t, tt.wantPID, e.BuilderPID)
		})
	}
}

// TestVerifiedBuildReportFailsClosed covers the success-path
// verification's fail-closed branches, which no admissible statement can
// reach through the public API on current server versions (the one shape
// that leaves an invalid entry on success — the concurrent
// partitioned-parent build — is refused by the server itself): an invalid
// entry under the build's name, and an unreadable catalog. Both must
// surface as *InvalidIndexError, never as a clean report.
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
