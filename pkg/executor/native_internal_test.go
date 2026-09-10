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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
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

// TestAsConcurrentBudgetError covers the cancellation partition, in
// precedence order. A server 57014 at or past the overall budget is the
// budget's own statement_timeout whatever the caller's context did. Below
// that, the caller's own context ending is the caller's cancellation in
// either mode and in either form it arrives. Under a live context SQLSTATE
// 57014 is query_canceled generally, so an early one is an external
// cancellation and must not read as budget exhaustion, because a consumer
// branching on *BudgetError escalates to a heavier strategy, the wrong
// reaction to a deliberate operator cancel.
func TestAsConcurrentBudgetError(t *testing.T) {
	budget := ConcurrentBudget{Overall: time.Minute}
	callerOwned := ConcurrentBudget{CallerOwned: true}
	cancelled := &pgconn.PgError{Code: sqlstateQueryCanceled}
	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	assertNotBudgetOrExternal := func(t *testing.T, err error) {
		t.Helper()
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "a cancellation that is not the budget's must not read as budget exhaustion")
		assert.NotErrorIs(t, err, ErrCancelledExternally)
	}

	t.Run("57014 at or past the budget is budget exhaustion", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, budget, time.Minute+time.Second, "concurrent index build")
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable behind the budget verdict")
		assert.Equal(t, sqlstateQueryCanceled, pgErr.Code)
	})

	// The budget's verdict outranks the caller's context: statement_timeout
	// fired at the configured limit, so the strategy was too slow whether or
	// not the caller also stopped waiting at the same instant. Reading it as
	// the caller's cancellation would hide the exhaustion the caller sized.
	t.Run("57014 at or past the budget under an ended context is still budget exhaustion", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, budget, time.Minute+time.Second, "concurrent index build")
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseStatement, budgetErr.Cause)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
	})

	// Only the server's own 57014 can be the budget's: pgx giving up on the
	// statement client-side is the caller's cancellation at any elapsed.
	t.Run("the client's own context error at or past the budget is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, context.DeadlineExceeded, budget, time.Minute+time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("57014 before the budget is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, budget, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "an external cancel must not read as budget exhaustion")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable for SQLSTATE branching")
		assert.Equal(t, sqlstateQueryCanceled, pgErr.Code)
	})

	t.Run("57014 under a live caller-owned context is an external cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(live, cancelled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})

	t.Run("57014 under an ended caller-owned context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server error must stay reachable for SQLSTATE branching")
	})

	t.Run("57014 under an ended bounded context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, cancelled, budget, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("the client's own context error under an ended context is the caller's cancellation", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, context.Canceled, callerOwned, 2*time.Second, "concurrent index build")
		require.ErrorIs(t, err, ErrCancelledByCaller)
		require.ErrorIs(t, err, context.Canceled, "the context error must stay reachable")
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("a non-cancellation failure under an ended context is an operational failure", func(t *testing.T) {
		err := asConcurrentBudgetError(ended, &pgconn.PgError{Code: "42P07"}, callerOwned, 2*time.Second, "concurrent index build")
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudgetOrExternal(t, err)
	})

	t.Run("any other failure is neither", func(t *testing.T) {
		err := asConcurrentBudgetError(live, &pgconn.PgError{Code: "42P07"}, budget, 2*time.Second, "concurrent index build")
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr))
	})
}

// TestIsStatementCancellationReadsEachFormOnItsOwn pins that the two forms
// a cancellation takes on the client are recognised independently: a
// server error of another kind sharing the chain with the client's own
// context error must not hide the context error, and a lone server error
// that is not query_canceled is not a cancellation.
func TestIsStatementCancellationReadsEachFormOnItsOwn(t *testing.T) {
	queryCanceled := &pgconn.PgError{Code: sqlstateQueryCanceled}
	adminShutdown := &pgconn.PgError{Code: "57P01"}
	connectionFailure := &pgconn.PgError{Code: "08006"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "server query_canceled", err: queryCanceled, want: true},
		{name: "wrapped server query_canceled", err: fmt.Errorf("exec: %w", queryCanceled), want: true},
		{name: "client context cancelled", err: context.Canceled, want: true},
		{name: "client deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "another server error ahead of the client's context error", err: fmt.Errorf("%w: %w", adminShutdown, context.Canceled), want: true},
		{name: "the client's context error ahead of another server error", err: fmt.Errorf("%w: %w", context.Canceled, adminShutdown), want: true},
		{name: "connection failure with the deadline", err: errors.Join(connectionFailure, context.DeadlineExceeded), want: true},
		{name: "another server error alone", err: adminShutdown, want: false},
		{name: "connection failure alone", err: connectionFailure, want: false},
		{name: "an untyped error", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isStatementCancellation(tt.err))
		})
	}
}

// cancelOnSecondRead is a clock that ends a context the second time it is
// read. The concurrent build reads its tracker's clock exactly twice — once
// before the statement, once the instant it returns — so the second read is
// the only point at which a caller can be made to cancel after a build has
// succeeded on the server and before its verdict runs.
type cancelOnSecondRead struct {
	cancel context.CancelFunc
	reads  int
}

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
		{name: "an entry the server will not drop concurrently does not", cleanup: ErrInvalidIndexNotDroppable},
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
// outranks droppability, droppability outranks visibility, and only a
// fully observable silence on a droppable entry on the target table is
// the caller's silence verdict.
func TestClassifyInvalidIndexOrdersByProofStrength(t *testing.T) {
	target := indexTarget{tableOID: 100, schema: "s"}
	observable := builderFacts{tracking: true}
	tests := []struct {
		name     string
		existing invalidIndex
		silence  error
		want     error
		wantPID  uint32
	}{
		{
			name:     "visible builder on the target table is in flight",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{pid: 42, tracking: true}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "visible builder on another table is still in flight",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", droppable: true, builder: builderFacts{pid: 42, hiddenRows: 3}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuildInFlight,
			wantPID:  42,
		},
		{
			name:     "another table's entry is refused before droppability is consulted",
			existing: invalidIndex{oid: 1, tableOID: 200, table: "u", droppable: false, builder: builderFacts{hiddenRows: 1}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexOnOtherTable,
		},
		{
			name:     "an entry the server will not drop concurrently is refused before visibility is consulted",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: false, builder: builderFacts{hiddenRows: 1}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexNotDroppable,
		},
		{
			name:     "a hidden command makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{hiddenRows: 1, tracking: true}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "activity tracking off makes the silence unobservable",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: builderFacts{tracking: false}},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrInvalidIndexBuilderUnobservable,
		},
		{
			name:     "observable silence on the target table before a build is abandoned",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: observable},
			silence:  ErrAbandonedInvalidIndex,
			want:     ErrAbandonedInvalidIndex,
		},
		{
			name:     "observable silence on the target table after this build's failure is its own leftover",
			existing: invalidIndex{oid: 1, tableOID: 100, table: "t", droppable: true, builder: observable},
			silence:  ErrBuildLeftInvalidIndex,
			want:     ErrBuildLeftInvalidIndex,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := classifyInvalidIndex(tt.existing, target, "i", tt.silence)
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
func TestDroppableColumnMatchesTheServer(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (id int PRIMARY KEY, u int UNIQUE, c int, e int, EXCLUDE USING btree (e WITH =));
		CREATE INDEX idx_plain ON %[1]s.t (c);
		CREATE UNIQUE INDEX idx_referenced ON %[1]s.t (c);
		CREATE TABLE %[1]s.r (id int PRIMARY KEY, t_c int REFERENCES %[1]s.t (c));
		CREATE TABLE %[1]s.p (id int, c int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.p1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
		CREATE INDEX idx_parent ON ONLY %[1]s.p (c);
		CREATE INDEX idx_partition ON %[1]s.p1 (c);
		ALTER INDEX %[1]s.idx_parent ATTACH PARTITION %[1]s.idx_partition`, schema))
	require.NoError(t, err)

	// The server refuses a partitioned table's index as unsupported, and
	// refuses an index some other object depends on — a constraint's, a
	// foreign key's referenced index, or a partition attached to a parent.
	const (
		sqlstateFeatureNotSupported        = "0A000"
		sqlstateDependentObjectsStillExist = "2BP01"
	)
	tests := []struct {
		name      string
		index     string
		droppable bool
		// refusal is the SQLSTATE the server answers DROP INDEX
		// CONCURRENTLY with when the predicate says not droppable.
		refusal string
	}{
		{name: "plain index", index: "idx_plain", droppable: true},
		{name: "primary key's index", index: "t_pkey", refusal: sqlstateDependentObjectsStillExist},
		{name: "unique constraint's index", index: "t_u_key", refusal: sqlstateDependentObjectsStillExist},
		{name: "exclusion constraint's index", index: "t_e_excl", refusal: sqlstateDependentObjectsStillExist},
		{name: "unique index a foreign key references", index: "idx_referenced", refusal: sqlstateDependentObjectsStillExist},
		{name: "partitioned table's index", index: "idx_parent", refusal: sqlstateFeatureNotSupported},
		{name: "attached index partition", index: "idx_partition", refusal: sqlstateDependentObjectsStillExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var droppable bool
			require.NoError(t, pool.QueryRow(t.Context(),
				`SELECT `+droppableColumn+`
				   FROM pg_catalog.pg_class c
				   JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
				  WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2`,
				schema, tt.index).Scan(&droppable))
			assert.Equal(t, tt.droppable, droppable, "the predicate's verdict")

			_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s", schema, tt.index))
			if tt.droppable {
				require.NoError(t, err, "the server drops what the predicate calls droppable")
				return
			}
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, "the server refuses what the predicate calls not droppable")
			assert.Equal(t, tt.refusal, pgErr.Code)
		})
	}
}

// A pooler that drops lock_timeout and statement_timeout from the startup
// packet leaves the pool to apply them as statements on each new session
// instead. RESET restores what the startup packet carried, so on such an
// endpoint it restores nothing and the session goes back to the pool with
// both bounds at zero. The release must therefore put the bounds back by
// value: a reused session without them is the unbounded state LK-2 exists
// to prevent.
func TestBudgetedSessionRestoresBoundsTheStartupPacketNeverCarried(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testutil.StartPostgres(t))
	require.NoError(t, err)
	// The pooler dropped them, so they are absent from the startup packet
	// and applied as statements — the arrangement this package's pool uses
	// against a session-mode pooler.
	delete(cfg.ConnConfig.RuntimeParams, "lock_timeout")
	delete(cfg.ConnConfig.RuntimeParams, "statement_timeout")
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET lock_timeout = 3000; SET statement_timeout = 30000")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	conn, release, err := acquireBudgetedSession(t.Context(), pool, ConcurrentBudget{Overall: time.Minute})
	require.NoError(t, err)
	var buildPID, duringLock int64
	require.NoError(t, conn.QueryRow(t.Context(),
		`SELECT pg_backend_pid(), (SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'lock_timeout')`).
		Scan(&buildPID, &duringLock))
	require.Zero(t, duringLock, "the build itself runs with lock_timeout disabled")
	release()

	reused, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer reused.Release()
	var reusedPID, lockMS, statementMS int64
	require.NoError(t, reused.QueryRow(t.Context(), `SELECT
		pg_backend_pid(),
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'lock_timeout'),
		(SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'statement_timeout')`).
		Scan(&reusedPID, &lockMS, &statementMS))
	require.Equal(t, buildPID, reusedPID,
		"the released session must be the one reused, or this proves nothing about it")
	assert.Equal(t, int64(3000), lockMS, "a reused session must not be left without a lock bound")
	assert.Equal(t, int64(30000), statementMS, "a reused session must not be left without a statement bound")
}
