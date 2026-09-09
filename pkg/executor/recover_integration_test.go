package executor_test

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
// returns once its invalid catalog entry exists. The build and the blocker
// are torn down at cleanup — so a failing assertion cannot leak either —
// and the teardown waits for the build to return.
func startBlockedBuild(t *testing.T, blockerPool, buildPool *pgxpool.Pool, schema, index string) {
	t.Helper()
	blocker, err := blockerPool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&n))

	buildCtx, cancelBuild := context.WithCancel(t.Context())
	done := make(chan error, 1)
	// Registered before the build starts, so the wait below failing still
	// tears the build and its blocker down rather than leaking them into
	// the pool's close.
	t.Cleanup(func() {
		cancelBuild()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Error("the cancelled build did not return")
		}
		require.NoError(t, blocker.Rollback(context.WithoutCancel(t.Context())))
	})
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

func TestDropAbandonedIndexRemovesOwnLeftoverWithoutRebuilding(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	leftover := indexOID(t, pool, schema, "idx_left")

	rep, err := executor.DropAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	require.Len(t, rep.Dropped, 1)
	assert.Equal(t, executor.DroppedIndex{
		Schema: schema, Index: quarantinedName(leftover), IndexOID: leftover, Duration: rep.Dropped[0].Duration,
	}, rep.Dropped[0])
	assert.Positive(t, rep.Dropped[0].Duration)
	assert.Empty(t, rep.Skipped)
	assert.Zero(t, rep.Build)
	assert.Positive(t, rep.Duration)
	exists, _ := indexState(t, pool, schema, "idx_left")
	assert.False(t, exists, "the requested index is not rebuilt")
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

func TestDropAbandonedIndexRefusesVisibleInFlightBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	startBlockedBuild(t, pool, pool, schema, "idx_busy")

	rep, err := executor.DropAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_busy ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrInvalidIndexBuildInFlight)
	assert.Zero(t, rep.Build)
	exists, valid := indexState(t, pool, schema, "idx_busy")
	assert.True(t, exists)
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

func TestDropAbandonedIndexRefusesOtherTableLeftover(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "a")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.b (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)
	leaveInvalidIndex(t, pool, schema, "a", "idx_shared")

	rep, err := executor.DropAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_shared ON %s.b (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrInvalidIndexOnOtherTable)
	assert.Zero(t, rep.Build)
	exists, valid := indexState(t, pool, schema, "idx_shared")
	assert.True(t, exists)
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

func TestDropAbandonedIndexWithoutDebrisDoesNotBuild(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, c int)", schema))
	require.NoError(t, err)

	rep, err := executor.DropAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema), buildBudget)
	require.NoError(t, err)

	assert.Empty(t, rep.Dropped)
	assert.Empty(t, rep.Skipped)
	assert.Zero(t, rep.Build)
	assert.Positive(t, rep.Duration)
	exists, _ := indexState(t, pool, schema, "idx_c")
	assert.False(t, exists)
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
	startBlockedBuild(t, pool, pool, schema, "idx_busy")

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

func TestRebuildAbandonedIndexRefusesSweepWhileQuarantinedEntryIsBeingBuilt(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	// The quarantine-named entry is another backend's build, visibly in
	// flight: a concurrent build parked in its snapshot wait holds no lock
	// on its own index, so the entry can be renamed under it, and the
	// progress view keeps reporting the build against the entry's OID.
	// (A REINDEX ... CONCURRENTLY of a quarantined entry is reported
	// against the new entry it builds, not the old one; the old one is
	// then guarded by the table lock the reindex holds, which the drop's
	// lock budget surfaces.)
	startBlockedBuild(t, pool, pool, schema, "idx_busy")
	busy := indexOID(t, pool, schema, "idx_busy")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.idx_busy RENAME TO %s", schema, quarantinedName(busy)))
	require.NoError(t, err)

	// The requested name is free, so the recovery reaches its sweep; the
	// sweep must refuse the entry someone is building rather than drop it
	// from under them, and must build nothing.
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.t (c)", schema), buildBudget)

	require.ErrorIs(t, err, executor.ErrInvalidIndexBuildInFlight)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, quarantinedName(busy), invalidErr.Index, "the refusal names the quarantined entry under build")
	assert.Equal(t, "t", invalidErr.Table)
	assert.Positive(t, invalidErr.BuilderPID, "the refusal names the building backend")
	assert.False(t, invalidErr.Recoverable())
	assert.Empty(t, rep.Dropped, "nothing is dropped while the entry is being built")
	assert.Equal(t, busy, indexOID(t, pool, schema, quarantinedName(busy)), "the entry under build survives")
	exists, _ := indexState(t, pool, schema, "idx_c")
	assert.False(t, exists, "the requested build does not start behind a refused sweep")
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

	startBlockedBuild(t, superuser, superuser, schema, "idx_hidden")

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

// createPartitionedTable makes a range-partitioned table with one partition
// holding duplicate rows, so a unique build on the partition fails after
// creating its catalog entry the way createTableWithDuplicates arranges for
// a plain table.
func createPartitionedTable(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.p (id int, c int) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.p1 PARTITION OF %[1]s.p FOR VALUES FROM (0) TO (100);
		INSERT INTO %[1]s.p VALUES (1, 7), (1, 7)`, schema))
	require.NoError(t, err)
}

func TestRebuildAbandonedIndexRefusesPartitionedParentIndex(t *testing.T) {
	pool, schema := newPool(t)
	createPartitionedTable(t, pool, schema)
	// A partitioned table's index built ON ONLY is invalid by design until
	// every partition's index is attached — the documented workflow, not
	// debris — and the server refuses to drop it concurrently.
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX idx_p ON ONLY %s.p (c)", schema))
	require.NoError(t, err)
	exists, valid := indexState(t, pool, schema, "idx_p")
	require.True(t, exists)
	require.False(t, valid, "the parent index must be invalid before its partitions are attached")
	parent := indexOID(t, pool, schema, "idx_p")

	stmt := fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_p ON %s.p (c)", schema)
	for name, run := range map[string]func() error{
		"build": func() error {
			_, err := executor.BuildIndexConcurrently(t.Context(), pool, stmt, buildBudget)
			return err
		},
		"recovery": func() error {
			_, err := executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)
			return err
		},
		"drop": func() error {
			_, err := executor.DropAbandonedIndex(t.Context(), pool, stmt, buildBudget)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			require.ErrorIs(t, err, executor.ErrInvalidIndexNotDroppable)
			var invalidErr *executor.InvalidIndexError
			require.ErrorAs(t, err, &invalidErr)
			assert.Equal(t, "idx_p", invalidErr.Index)
			assert.Equal(t, "p", invalidErr.Table)
			assert.False(t, invalidErr.Recoverable(), "an entry the server will not drop concurrently is an operator's, never the recovery's")
			assert.Equal(t, parent, indexOID(t, pool, schema, "idx_p"), "the parent index is the same catalog entry")
			assert.Empty(t, quarantinedIndexes(t, pool, schema), "nothing is quarantined")
		})
	}

	// The refused entry is still the workflow's own: attaching the
	// partition's index completes it.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE INDEX p1_c ON %[1]s.p1 (c); ALTER INDEX %[1]s.idx_p ATTACH PARTITION %[1]s.p1_c", schema))
	require.NoError(t, err)
	_, valid = indexState(t, pool, schema, "idx_p")
	assert.True(t, valid, "the parent index becomes valid once every partition's index is attached")
}

func TestRebuildAbandonedIndexSkipsQuarantinedEntryTheServerWillNotDrop(t *testing.T) {
	pool, schema := newPool(t)
	createPartitionedTable(t, pool, schema)
	// A recovery quarantined a failed unique build's leftover on the
	// partition and died before its drop; an operator then attached the
	// entry to a partitioned parent index, and the server now refuses to
	// drop it concurrently. A unique index on a partitioned table must
	// include the partition key, and an attached partition index must
	// match its parent's definition, so both are unique on (id, c).
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX idx_p ON ONLY %s.p (id, c)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_leaf ON %s.p1 (id, c)", schema))
	require.Error(t, err, "a unique build over duplicates must fail")
	debris := indexOID(t, pool, schema, "idx_leaf")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"ALTER INDEX %[1]s.idx_leaf RENAME TO %[2]s; ALTER INDEX %[1]s.idx_p ATTACH PARTITION %[1]s.%[2]s",
		schema, quarantinedName(debris)))
	require.NoError(t, err)
	_, valid := indexState(t, pool, schema, quarantinedName(debris))
	require.False(t, valid, "the attached entry stays invalid")

	// The plain build does not refuse over an entry the recovery could not
	// remove anyway: a permanent refusal would wedge every later build on
	// the partition.
	stmt := fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_c ON %s.p1 (c)", schema)
	built, err := executor.BuildIndexConcurrently(t.Context(), pool, stmt, buildBudget)
	require.NoError(t, err)
	assert.Equal(t, "idx_c", built.Index)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX %s.idx_c", schema))
	require.NoError(t, err)

	// The recovery steps over the entry, reports it, and builds.
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)
	require.NoError(t, err)
	assert.Empty(t, rep.Dropped, "nothing the server refuses to drop is attempted")
	require.Len(t, rep.Skipped, 1, "the entry left in place is reported")
	assert.Equal(t, schema, rep.Skipped[0].Schema)
	assert.Equal(t, quarantinedName(debris), rep.Skipped[0].Index)
	assert.Equal(t, debris, rep.Skipped[0].IndexOID)
	assert.Equal(t, "idx_c", rep.Build.Index)
	assert.Positive(t, rep.Duration)
	exists, valid := indexState(t, pool, schema, "idx_c")
	assert.True(t, exists)
	assert.True(t, valid)
	assert.Equal(t, []string{quarantinedName(debris)}, quarantinedIndexes(t, pool, schema),
		"the skipped entry survives for an operator")
}

func TestBuildIndexConcurrentlyRefusesQuarantinedDebris(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	// A recovery that died between its rename and its drop: the requested
	// name is free, and the debris sits on the table under its quarantine
	// name. A build that succeeded beside it would be the last anyone
	// heard of the debris, so the build refuses and names the entry.
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	debris := indexOID(t, pool, schema, "idx_left")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.idx_left RENAME TO %s", schema, quarantinedName(debris)))
	require.NoError(t, err)

	stmt := fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema)
	_, err = executor.BuildIndexConcurrently(t.Context(), pool, stmt, buildBudget)

	require.ErrorIs(t, err, executor.ErrAbandonedInvalidIndex)
	var invalidErr *executor.InvalidIndexError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, quarantinedName(debris), invalidErr.Index, "the refusal names the entry that is invalid, not the free name")
	assert.Equal(t, "t", invalidErr.Table)
	assert.True(t, invalidErr.Recoverable())
	exists, _ := indexState(t, pool, schema, "idx_left")
	assert.False(t, exists, "the refusal precedes any execution")

	// The recovery the refusal names sweeps the debris and builds.
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)
	require.NoError(t, err)
	require.Len(t, rep.Dropped, 1)
	assert.Equal(t, debris, rep.Dropped[0].IndexOID)
	assert.Equal(t, "idx_left", rep.Build.Index)
}

func TestRebuildAbandonedIndexReportsLockBudgetWhenDropIsBlocked(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	// Quarantined debris with the requested name free: the recovery goes
	// straight to its sweep, and the sweep's DROP INDEX CONCURRENTLY needs
	// the table lock a concurrent index command — visible or not — holds.
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	debris := indexOID(t, pool, schema, "idx_left")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.idx_left RENAME TO %s", schema, quarantinedName(debris)))
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	start := time.Now()
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
	elapsed := time.Since(start)

	var budgetErr *executor.BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, executor.CauseLock, budgetErr.Cause, "the drop gave up on the table lock, not the statement budget")
	assert.Positive(t, budgetErr.Budget)
	assert.Less(t, budgetErr.Budget, buildBudget.Overall, "the drop's lock bound is its own, shorter than the sweep's budget")
	assert.GreaterOrEqual(t, elapsed, budgetErr.Budget, "the drop waited out its bound before giving up")
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr, "a lock not granted is a budget outcome, not a verdict on the entry")
	assert.Empty(t, rep.Dropped)
	assert.Equal(t, []string{quarantinedName(debris)}, quarantinedIndexes(t, pool, schema),
		"the debris survives exactly as quarantined for the next sweep")
	_, valid := indexState(t, pool, schema, quarantinedName(debris))
	assert.False(t, valid)
	exists, _ := indexState(t, pool, schema, "idx_left")
	assert.False(t, exists, "the build never ran")
}

// TestRebuildAbandonedIndexReportsItsCallerCancellingTheDrop covers the
// most ordinary way a sweep ends under an orchestrator: the caller's lease
// lapses while a drop waits on the table lock. The sweep must report the
// caller's own cancellation as itself — not as a verdict on the entry,
// which a cancelled drop leaves exactly as quarantined for the next sweep.
func TestRebuildAbandonedIndexReportsItsCallerCancellingTheDrop(t *testing.T) {
	pool, schema := newPool(t)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	debris := indexOID(t, pool, schema, "idx_left")
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.idx_left RENAME TO %s", schema, quarantinedName(debris)))
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.t IN SHARE UPDATE EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	var rep executor.IndexRecoveryReport
	go func() {
		var sweepErr error
		rep, sweepErr = executor.RebuildAbandonedIndex(ctx, pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema), buildBudget)
		done <- sweepErr
	}()
	require.Eventually(t, func() bool {
		var waiting int
		err := pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_catalog.pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE 'DROP INDEX CONCURRENTLY %'`).Scan(&waiting)
		return err == nil && waiting == 1
	}, 10*time.Second, 20*time.Millisecond, "the drop must be waiting on the held table lock")
	cancel()

	err = <-done
	require.ErrorIs(t, err, executor.ErrCancelledByCaller)
	assert.NotErrorIs(t, err, executor.ErrCancelledExternally)
	var budgetErr *executor.BudgetError
	assert.False(t, errors.As(err, &budgetErr), "the caller stopped the drop before its lock bound")
	var invalidErr *executor.InvalidIndexError
	require.NotErrorAs(t, err, &invalidErr, "the caller's own cancellation is not a verdict on the entry")
	assert.Equal(t, executor.CodeCancelledByCaller, executor.OutcomeCode(err))
	assert.Empty(t, rep.Dropped)
	assert.Equal(t, []string{quarantinedName(debris)}, quarantinedIndexes(t, pool, schema),
		"the debris survives exactly as quarantined for the next sweep")
	exists, _ := indexState(t, pool, schema, "idx_left")
	assert.False(t, exists, "the build never ran")
}

// TestRebuildAbandonedIndexRefusesPoolWithoutRoomForItsSession covers the
// recovery's admission-time pool guard: its own session stays open across
// the drops and the build, so a pool sized for the build alone would not
// fail but wait on itself; the recovery refuses it before anything runs.
func TestRebuildAbandonedIndexRefusesPoolWithoutRoomForItsSession(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	stmt := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema)

	// The same pool is enough for the build alone.
	_, err = executor.BuildIndexConcurrently(t.Context(), pool, stmt, buildBudget)
	require.ErrorIs(t, err, executor.ErrAbandonedInvalidIndex, "two connections admit the build")

	_, err = executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)

	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	assert.Empty(t, quarantinedIndexes(t, pool, schema), "the refusal precedes any execution")
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.False(t, valid)
}

// TestDropAbandonedIndexRunsOnPoolSizedForTheBuild pins the drop-only
// recovery's smaller pool minimum: with no build to run, its own session
// plus one drop session is the whole peak, so a pool the rebuild refuses
// admits the drop and the drop completes on it rather than waiting on
// itself for a connection.
func TestDropAbandonedIndexRunsOnPoolSizedForTheBuild(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	leftover := indexOID(t, pool, schema, "idx_left")
	stmt := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema)

	_, err = executor.RebuildAbandonedIndex(t.Context(), pool, stmt, buildBudget)
	require.ErrorIs(t, err, executor.ErrPoolTooSmall, "the same pool is too small for a rebuild")

	rep, err := executor.DropAbandonedIndex(t.Context(), pool, stmt, buildBudget)
	require.NoError(t, err)

	require.Len(t, rep.Dropped, 1)
	assert.Equal(t, leftover, rep.Dropped[0].IndexOID)
	assert.Empty(t, rep.Skipped)
	assert.Zero(t, rep.Build)
	exists, _ := indexState(t, pool, schema, "idx_left")
	assert.False(t, exists)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}

// TestDropAbandonedIndexRefusesSingleConnectionPool pins the lower bound of
// the drop-only recovery's pool minimum: the drop session runs while the
// recovery session is held, so a pool of one connection would wait on
// itself for the drop instead of failing. The recovery refuses it before
// any session use, and the leftover stands untouched.
func TestDropAbandonedIndexRefusesSingleConnectionPool(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	createTableWithDuplicates(t, pool, schema, "t")
	leaveInvalidIndex(t, pool, schema, "t", "idx_left")
	leftover := indexOID(t, pool, schema, "idx_left")
	stmt := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema)

	rep, err := executor.DropAbandonedIndex(t.Context(), pool, stmt, buildBudget)

	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	assert.Empty(t, rep.Dropped)
	assert.Empty(t, rep.Skipped)
	assert.Equal(t, leftover, indexOID(t, pool, schema, "idx_left"), "the refusal precedes any execution")
	exists, valid := indexState(t, pool, schema, "idx_left")
	assert.True(t, exists)
	assert.False(t, valid)
	assert.Empty(t, quarantinedIndexes(t, pool, schema))
}
