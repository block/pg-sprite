package executor_test

import (
	"context"
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
	"github.com/block/pg-sprite/pkg/executor"
)

// quarantinedName is the identity-derived name the recovery gives an
// abandoned entry before dropping it: the contract a crashed recovery's
// debris is recognised by, so tests spell it out rather than import it.
func quarantinedName(oid uint32) string {
	return fmt.Sprintf("pgsprite_abandoned_%d", oid)
}

// indexOID returns the catalog identity of the named index.
func indexOID(t *testing.T, pool *pgxpool.Pool, schema, index string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT c.oid FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`, schema, index).Scan(&oid))
	return oid
}

// quarantinedIndexes lists every index in the schema carrying the
// quarantine prefix, whatever its validity.
func quarantinedIndexes(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT c.relname FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind = 'i' AND c.relname LIKE 'pgsprite_abandoned_%'
		  ORDER BY c.relname`, schema)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

// createTableWithDuplicates makes the fixture every abandonment test
// starts from: a table whose duplicate values make a unique build fail
// after creating its catalog entry.
func createTableWithDuplicates(t *testing.T, pool *pgxpool.Pool, schema, table string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.%[2]s (id int PRIMARY KEY, c int); INSERT INTO %[1]s.%[2]s VALUES (1, 7), (2, 7)",
		schema, table))
	require.NoError(t, err)
}

// startBlockedBuild runs a caller-owned concurrent build in the background
// against a repeatable-read snapshot that holds it in its wait phase, and
// returns once its invalid catalog entry exists. The returned stop tears
// the build and the blocker down and waits for the build to return.
func startBlockedBuild(t *testing.T, blockerPool, buildPool *pgxpool.Pool, schema, index string) (stop func()) {
	t.Helper()
	blocker, err := blockerPool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))

	buildCtx, cancelBuild := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := executor.BuildIndexConcurrently(buildCtx, buildPool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s.t (c)", index, schema),
			executor.ConcurrentBudget{CallerOwned: true})
		done <- err
	}()
	require.Eventually(t, func() bool {
		exists, _ := indexState(t, blockerPool, schema, index)
		return exists
	}, 30*time.Second, 50*time.Millisecond, "the blocked build must have created its catalog entry")

	return func() {
		cancelBuild()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Error("the cancelled build did not return")
		}
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	}
}

func TestRebuildAbandonedIndexRemovesOwnLeftoverAndRebuilds(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	leftover := indexOID(t, pool, schema, "idx_left")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("DELETE FROM %s.t WHERE id = 2", schema))
	require.NoError(t, err)

	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	require.Len(t, rep.Dropped, 1, "exactly the abandoned entry is removed")
	assert.Equal(t, schema, rep.Dropped[0].Schema)
	assert.Equal(t, quarantinedName(leftover), rep.Dropped[0].Index, "the drop names the entry by its identity, not the build's name")
	assert.Equal(t, leftover, rep.Dropped[0].IndexOID)
	assert.Positive(t, rep.Dropped[0].Duration)
	assert.Equal(t, schema, rep.Build.Schema)
	assert.Equal(t, "idx_left", rep.Build.Index)
	assert.NotEqual(t, leftover, rep.Build.IndexOID, "the rebuilt index is a new catalog entry")
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.True(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema), "no quarantined debris survives a completed recovery")
}

func TestRebuildAbandonedIndexWithoutDebrisIsJustTheBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	assert.Empty(t, rep.Dropped)
	assert.Equal(t, "idx_c", rep.Build.Index)
	assert.NotZero(t, rep.Build.IndexOID)
	exists, valid := indexState(t, pool, schema, "idx_c")
	assert.True(t, exists)
	assert.True(t, valid)
}

func TestRebuildAbandonedIndexRefusesVisibleInFlightBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	stop := startBlockedBuild(t, pool, pool, schema, "idx_busy")
	defer stop()

	// The entry under the name is another backend's build, visibly in
	// progress: the recovery must refuse before taking any lock, and the
	// entry must keep its name for that build to finish under.
	_, err = executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_busy ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrInvalidIndexBuildInFlight)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Positive(t, invalidErr.BuilderPID, "the refusal names the building backend")
	assert.Equal(t, "t", invalidErr.Table)
	assert.False(t, invalidErr.Recoverable())
	exists, valid := indexState(t, pool, schema, "idx_busy")
	assert.True(t, exists, "the in-flight build's entry must survive under its own name")
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema), "nothing is quarantined while a build is in flight")
}

func TestRebuildAbandonedIndexRefusesOtherTableLeftover(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "a")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.b (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	leaveInvalidIndex(t, pool, schema, "a", "idx_shared")

	_, err = executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_shared ON %s.b (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrInvalidIndexOnOtherTable)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, "a", invalidErr.Table)
	exists, valid := indexState(t, pool, schema, "idx_shared")
	assert.True(t, exists, "another table's leftover must survive untouched")
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

func TestRebuildAbandonedIndexReportsLockBudgetWhenTableIsBusy(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")

	// A held SHARE UPDATE EXCLUSIVE lock is what any concurrent index
	// command on the table looks like from the outside — including one
	// this role cannot observe. The proof lock must give up within its
	// bound and leave the entry exactly as found.
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	start := time.Now()
	_, err = executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	elapsed := time.Since(start)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	assert.Positive(t, budgetErr.Budget)
	assert.GreaterOrEqual(t, elapsed, budgetErr.Budget, "the proof waited out its bound before giving up")
	assert.Less(t, elapsed, 2*budgetErr.Budget, "the proof gave up at its bound, not behind the lock")
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr, "an unproven lock is a budget outcome, not a verdict on the entry")
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists, "the entry must survive under its original name")
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

func TestRebuildAbandonedIndexSweepsQuarantinedDebris(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")

	// A recovery that died between its rename and its drop leaves an
	// entry under its quarantine name; the requested name is free again.
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	orphan := indexOID(t, pool, schema, "idx_left")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.idx_left RENAME TO %s", schema, quarantinedName(orphan)))
	require.NoError(t, err)
	// An operator's own invalid index that merely starts with the prefix is
	// not quarantine debris: its name is not derived from its OID.
	leaveInvalidIndex(t, pool, schema, "t", "pgsprite_abandoned_1")
	lookalike := indexOID(t, pool, schema, "pgsprite_abandoned_1")
	require.NotEqual(t, uint32(1), lookalike)

	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	require.Len(t, rep.Dropped, 1, "the sweep removes the orphaned quarantine entry and nothing else")
	assert.Equal(t, orphan, rep.Dropped[0].IndexOID)
	assert.Equal(t, quarantinedName(orphan), rep.Dropped[0].Index)
	assert.Equal(t, "idx_left", rep.Build.Index)
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.True(t, valid)
	assert.Equal(t, []string{"pgsprite_abandoned_1"}, quarantinedIndexes(t, pool, schema),
		"the lookalike survives; only names derived from their own OID are swept")
}

func TestRebuildAbandonedIndexNeverDropsValidIndex(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.t (id int PRIMARY KEY, c int); CREATE INDEX idx_v ON %s.t (c)", schema, schema))
	require.NoError(t, err)
	before := indexOID(t, pool, schema, "idx_v")

	// A valid index under the name is not debris of any kind: the recovery
	// has nothing to remove, and the build fails on the name the way it
	// would without the recovery.
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_v ON %s.t (c)", schema), buildBudget)

	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, sqlstateDuplicateTable, pgErr.Code)
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr)
	assert.Empty(t, rep.Dropped)
	assert.Equal(t, before, indexOID(t, pool, schema, "idx_v"), "the valid index is the same catalog entry")
	exists, valid := indexState(t, pool, schema, "idx_v")
	assert.True(t, exists)
	assert.True(t, valid)
}

func TestRebuildAbandonedIndexHiddenBuilderIsUnobservableUntilLockProven(t *testing.T) {
	url := testutil.StartPostgres(t)
	superuser, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(superuser.Close)
	schema := testutil.NewSchema(t, superuser)

	// A role without pg_read_all_stats sees another role's progress row
	// with its target columns nulled: it can tell a command is running
	// somewhere in the database, not which index it is building.
	role := "limited_" + schema
	_, err = superuser.Exec(t.Context(), fmt.Sprintf(`
		CREATE ROLE %[1]s LOGIN PASSWORD 'limited';
		GRANT USAGE, CREATE ON SCHEMA %[2]s TO %[1]s;
		CREATE TABLE %[2]s.t (id int PRIMARY KEY, c int);
		ALTER TABLE %[2]s.t OWNER TO %[1]s`, role, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := superuser.Exec(ctx, fmt.Sprintf("DROP OWNED BY %[1]s; DROP ROLE %[1]s", role))
		if err != nil {
			t.Logf("drop role %s: %v", role, err)
		}
	})
	limited, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL: url,
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.User = role
			cc.Password = "limited"
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(limited.Close)

	stop := startBlockedBuild(t, superuser, superuser, schema, "idx_hidden")
	defer stop()

	// The build sees an invalid entry, no visible builder, and a hidden
	// progress row: it must not call that abandoned.
	stmt := fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_hidden ON %s.t (c)", schema)
	_, err = executor.BuildIndexConcurrently(t.Context(), limited, stmt, buildBudget)
	require.ErrorIs(t, err, executor.ErrInvalidIndexBuilderUnobservable)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Zero(t, invalidErr.BuilderPID)
	assert.Equal(t, "t", invalidErr.Table)
	assert.True(t, invalidErr.Recoverable(), "unobservable is recoverable: the lock proof decides")

	// The recovery's lock proof is what the hidden builder cannot hide
	// from: it holds the table lock, so the proof reports its budget and
	// leaves the entry alone.
	_, err = executor.RebuildAbandonedIndex(t.Context(), limited, stmt, buildBudget)
	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause)
	exists, valid := indexState(t, superuser, schema, "idx_hidden")
	assert.True(t, exists, "the hidden build's entry must survive under its own name")
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, superuser, schema))

	// Granted the stats privilege, the same role sees the builder.
	_, err = superuser.Exec(t.Context(), fmt.Sprintf("GRANT pg_read_all_stats TO %s", role))
	require.NoError(t, err)
	_, err = executor.BuildIndexConcurrently(t.Context(), limited, stmt, buildBudget)
	require.ErrorIs(t, err, executor.ErrInvalidIndexBuildInFlight)
	require.ErrorAs(t, err, &invalidErr)
	assert.Positive(t, invalidErr.BuilderPID)
}

func TestRebuildAbandonedIndexWithActivityTrackingOffIsUnobservableYetRecoverable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL: testutil.StartPostgres(t),
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.RuntimeParams["track_activities"] = "off"
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DELETE FROM %s.t WHERE id = 2", schema))
	require.NoError(t, err)

	// With activity tracking off no command reports progress, so the
	// builder subquery's silence proves nothing and the build refuses as
	// unobservable rather than abandoned.
	stmt := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema)
	_, err = executor.BuildIndexConcurrently(t.Context(), pool, stmt, buildBudget)
	require.ErrorIs(t, err, executor.ErrInvalidIndexBuilderUnobservable)

	// The recovery does not need the progress view: nobody holds the
	// table lock, so the proof succeeds and the entry is removed.
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)
	require.NoError(t, err)
	require.Len(t, rep.Dropped, 1)
	assert.Equal(t, "idx_left", rep.Build.Index)
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.True(t, valid)
}
