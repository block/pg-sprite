package migrate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/verdict"
)

// progressPool is a pool for the tracked runs here, and a schema of its
// own with the table each test changes.
func progressPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.orders (
			id   int PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.orders (id, note)
		SELECT g, 'note ' || g
		FROM generate_series(1, 1000) g`, schema))
	require.NoError(t, err)
	return pool, schema
}

// holdOldSnapshot opens a repeatable-read transaction whose snapshot
// predates any concurrent index build started after it, so the build waits
// in its "waiting for old snapshots" phase until release is called. The
// snapshot is taken by reading pg_class, not the table, so the transaction
// holds no lock a statement on the table would wait for.
func holdOldSnapshot(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM pg_catalog.pg_class").Scan(&n))
	released := false
	release = func() {
		if !released {
			released = true
			assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
		}
	}
	t.Cleanup(release)
	return release
}

// awaitBuildWaitingForSnapshots polls tracker until the build it reports
// is waiting for the held snapshot, and returns that snapshot.
func awaitBuildWaitingForSnapshots(t *testing.T, tracker *progress.Tracker) progress.Snapshot {
	t.Helper()
	const buildWaitDeadline = 30 * time.Second
	var observed progress.Snapshot
	require.Eventually(t, func() bool {
		snapshot, err := tracker.Progress(t.Context())
		if err != nil || snapshot.Detail.Work == nil {
			return false
		}
		observed = snapshot
		return snapshot.Detail.Work.LockersTotal >= 1
	}, buildWaitDeadline, 10*time.Millisecond, "the build must reach the wait for the held snapshot")
	return observed
}

// A tracker handed to Run sees the statement as step 1 of 1, and while a
// concurrent index build runs, the server's own progress for it: the
// operation, PostgreSQL's phase, and the build counters. The build is held
// in its wait for an older snapshot, so the poll lands on one known phase.
// Once Run returns the tracker is finished and keeps the step it ended on.
func TestRunReportsAConcurrentIndexBuildToTheTracker(t *testing.T) {
	pool, schema := progressPool(t)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := runOptions()
	opts.Progress = tracker
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // registered first, so it runs after the snapshot is released
	release := holdOldSnapshot(t, pool)

	st := parseOne(t, fmt.Sprintf("CREATE INDEX CONCURRENTLY orders_note_idx ON %s.orders (note)", schema))
	type result struct {
		v   verdict.Verdict
		err error
	}
	done := make(chan result, 1)
	wg.Go(func() {
		v, runErr := migrate.Run(t.Context(), pool, st, opts)
		done <- result{v: v, err: runErr}
	})

	mid := awaitBuildWaitingForSnapshots(t, tracker)
	release()

	assert.Equal(t, progress.PhaseRunning, mid.Phase)
	assert.Equal(t, 1, mid.Step)
	assert.Equal(t, 1, mid.TotalSteps, "Run is a run of one statement")
	assert.Equal(t, progress.OperationConcurrentIndex, mid.Detail.Operation)
	assert.Equal(t, fmt.Sprintf("CREATE INDEX CONCURRENTLY orders_note_idx ON %s.orders USING btree (note)", schema), mid.Detail.Statement,
		"the detail names the canonical statement the build runs")
	assert.Equal(t, "waiting for old snapshots", mid.Detail.ServerPhase, "PostgreSQL's own phase for the held build")
	assert.True(t, mid.Detail.Active)
	assert.GreaterOrEqual(t, mid.Detail.Work.LockersTotal, uint64(1),
		"the held snapshot is among those the build waits for; on a shared database other sessions' may be too")

	r := <-done
	require.NoError(t, r.err)
	assert.Equal(t, verdict.OutcomeExecuted, r.v.Outcome)
	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, end.Phase)
	assert.Equal(t, 1, end.Step)
	assert.Equal(t, progress.OperationConcurrentIndex, end.Detail.Operation)
	assert.False(t, end.Detail.Active)
}

// A tracker handed to RunDesired sees the planned statements as its steps:
// the plan here adds a column and then builds an index, so the build is
// step 2 of 2, reported with the server's progress for it, and the tracker
// is finished once the run has converged.
func TestRunDesiredReportsEachPlannedStatementAsAStep(t *testing.T) {
	pool, schema := progressPool(t)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := runOptions()
	opts.Progress = tracker
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // registered first, so it runs after the snapshot is released
	release := holdOldSnapshot(t, pool)

	desired := parseDesired(t, `
		CREATE TABLE orders (
			id    int PRIMARY KEY,
			note  text,
			notes int
		);
		CREATE INDEX orders_note_idx ON orders (note);`)
	type result struct {
		res migrate.DesiredResult
		err error
	}
	done := make(chan result, 1)
	wg.Go(func() {
		res, runErr := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, opts)
		done <- result{res: res, err: runErr}
	})

	mid := awaitBuildWaitingForSnapshots(t, tracker)
	release()

	assert.Equal(t, progress.PhaseRunning, mid.Phase)
	assert.Equal(t, 2, mid.Step, "the index build is the plan's second statement")
	assert.Equal(t, 2, mid.TotalSteps, "one step per planned statement")
	assert.Equal(t, progress.OperationConcurrentIndex, mid.Detail.Operation)
	assert.Equal(t, "waiting for old snapshots", mid.Detail.ServerPhase)
	assert.GreaterOrEqual(t, mid.Detail.Work.LockersTotal, uint64(1))

	r := <-done
	require.NoError(t, r.err)
	assert.Equal(t, verdict.OutcomeExecuted, r.res.Outcome)
	require.Len(t, r.res.Plan.Statements, 2)
	assert.Equal(t, fmt.Sprintf("CREATE INDEX orders_note_idx ON %s.orders USING btree (note)", schema), r.res.Plan.Statements[1].SQL)
	assert.Equal(t, fmt.Sprintf("CREATE INDEX CONCURRENTLY orders_note_idx ON %s.orders USING btree (note)", schema), mid.Detail.Statement,
		"step 2 reports the statement the plan's second statement runs as: its concurrent build")
	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, end.Phase)
	assert.Equal(t, 2, end.Step)
}

// An index rename runs as one bounded attempt outside the sequence
// executor, so Run reports it as the step itself: step 1, operation
// optimistic, the statement as executed.
func TestRunReportsAnIndexRenameAsItsStep(t *testing.T) {
	pool, schema := progressPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE INDEX orders_note_idx ON %s.orders (note)`, schema))
	require.NoError(t, err)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := runOptions()
	opts.Progress = tracker

	sql := fmt.Sprintf(`ALTER INDEX %s.orders_note_idx RENAME TO orders_remark_idx`, schema)
	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, v.Outcome)

	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, end.Phase)
	assert.Equal(t, 1, end.Step)
	assert.Equal(t, 1, end.TotalSteps)
	assert.Equal(t, progress.OperationOptimistic, end.Detail.Operation)
	assert.Equal(t, sql, end.Detail.Statement)
}

// A desired file for a table that does not exist runs as a create, whose
// statements are the plan's: the CREATE TABLE is step 1 and the index
// step 2, each a brief step, and the tracker finishes on the last.
func TestRunDesiredReportsACreatesStatementsAsSteps(t *testing.T) {
	pool, schema := progressPool(t)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := runOptions()
	opts.Progress = tracker

	desired := parseDesired(t, `
		CREATE TABLE invoices (
			id    int PRIMARY KEY,
			total int
		);
		CREATE INDEX invoices_total_idx ON invoices (total);`)
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, opts)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	require.Len(t, res.Plan.Statements, 2)

	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFinished, end.Phase)
	assert.Equal(t, 2, end.Step)
	assert.Equal(t, 2, end.TotalSteps)
	assert.Equal(t, progress.OperationBrief, end.Detail.Operation)
	assert.Equal(t, fmt.Sprintf("CREATE INDEX invoices_total_idx ON %s.invoices USING btree (total)", schema), end.Detail.Statement)
}

// A refused run ends its tracker failed, not finished: Run returns a
// refusal verdict with no error, but the statement did not execute. Here
// the statement drops a column without the destructive acknowledgement, so
// it is refused before any route runs it.
func TestRunEndsTheTrackerFailedWhenTheStatementIsRefused(t *testing.T) {
	pool, schema := progressPool(t)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	opts := runOptions()
	opts.Progress = tracker

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders DROP COLUMN note", schema)), opts)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	require.Equal(t, verdict.ReasonDestructiveChange, v.Reason)

	end, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseFailed, end.Phase)
	assert.Equal(t, progress.OperationAdmitting, end.Detail.Operation, "nothing executed, so no step started")
	assert.Zero(t, end.Step)
}
