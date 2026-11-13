package testutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGenerator(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 200))
	toastBytes, err := table.ToastBytes(t.Context())
	require.NoError(t, err)
	assert.Positive(t, toastBytes, "blob values must be stored out of line for the unchanged-TOAST update profile to mean anything")
	generator := StartLoad(t, pool, table, LoadSpec{
		Seed: 42, Workers: 4, RatePerSecond: 80,
		Mix:            Mix{Insert: 1, Update: 1, Delete: 1, UniqueMove: 1},
		HotRowFraction: 0.2, ToastRewriteFraction: 0.5,
	})
	runDeadline := time.NewTimer(2 * time.Second)
	defer runDeadline.Stop()
	<-runDeadline.C
	running := generator.Snapshot()
	summary, err := generator.Stop()
	require.NoError(t, err)
	assert.Positive(t, summary.Inserts)
	assert.Positive(t, summary.Updates)
	assert.Positive(t, summary.Deletes)
	assert.Positive(t, summary.UniqueMoves)
	committed := summary.Inserts + summary.Updates + summary.Deletes + summary.UniqueMoves
	assert.Equal(t, committed, summary.Committed())
	assert.Less(t, summary.Races, committed, "a run should commit far more than it aborts on expected races")
	assert.Positive(t, running.Committed(), "a snapshot of the running load reports what it had committed")
	assert.LessOrEqual(t, running.Committed(), summary.Committed(), "the load only adds to what a snapshot reported")
	require.LessOrEqual(t, len(running.InsertedIDs), len(summary.InsertedIDs))
	assert.Equal(t, running.InsertedIDs, summary.InsertedIDs[:len(running.InsertedIDs)], "a snapshot's inserted ids are the start of the final list")
	// Every committed mutation touches at least one row and a unique-key
	// swap touches two, so the touched list is at least as long as the
	// commit count and grows in commit order.
	assert.GreaterOrEqual(t, len(summary.Touches), summary.Committed())
	assert.Equal(t, summary.Inserts+summary.Updates+summary.Deletes+2*summary.UniqueMoves, len(summary.Touches))
	require.LessOrEqual(t, len(running.Touches), len(summary.Touches))
	assert.Equal(t, running.Touches, summary.Touches[:len(running.Touches)], "a snapshot's touches are the start of the final list")
	for _, touch := range summary.Touches {
		assert.Positive(t, touch.Position, "every touch carries the WAL position its mutation read")
	}
	assert.Empty(t, summary.Ambiguous, "no mutation ran out its deadline on an uncontended table")
	assert.Subset(t, summary.TouchedIDs(), summary.InsertedIDs)
	assert.Subset(t, summary.TouchedIDs(), summary.DeletedIDs)
	assert.Positive(t, summary.MaxLatency)
	assert.Less(t, summary.MaxLatency, MutationTimeout, "a committed mutation finished inside its deadline")
	assert.Empty(t, summary.Failures, "a generator that does not record failures never lists any")
	assert.Equal(t, Progress{
		Inserts:     summary.Inserts,
		Updates:     summary.Updates,
		Deletes:     summary.Deletes,
		UniqueMoves: summary.UniqueMoves,
		Touched:     len(summary.Touches),
	}, generator.Progress(), "a stopped load's progress is its summary's counts")
	var count, distinct int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT uniq) FROM `+table.Qualified()).Scan(&count, &distinct))
	assert.Equal(t, 200+summary.Inserts-summary.Deletes, count)
	assert.Equal(t, count, distinct)
}

// A check constraint that every update violates fails each mutation with
// a server error that is not an expected race. A generator that records
// failures keeps its worker ticking and lists every one with its SQLSTATE,
// so Stop reports no error and the summary shows what the workload could
// not commit; a generator that does not record them stops on the first.
func TestLoadGeneratorRecordsFailures(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 20))
	_, err = pool.Exec(t.Context(), `ALTER TABLE `+table.Qualified()+` ADD CONSTRAINT label_never_updated CHECK (label NOT LIKE '%u')`)
	require.NoError(t, err)

	recording := StartLoad(t, pool, table, LoadSpec{Seed: 3, Workers: 1, RatePerSecond: 50, Mix: Mix{Update: 1}, RecordFailures: true})
	runDeadline := time.NewTimer(time.Second)
	defer runDeadline.Stop()
	<-runDeadline.C
	summary, err := recording.Stop()
	require.NoError(t, err)
	assert.Zero(t, summary.Committed())
	assert.Empty(t, summary.Touches)
	require.Greater(t, len(summary.Failures), 1, "the worker must keep ticking after a recorded failure")
	for _, failure := range summary.Failures {
		assert.Equal(t, "23514", failure.Code, "check_violation is the recorded SQLSTATE")
		assert.Error(t, failure.Err)
		assert.False(t, failure.At.IsZero())
	}
	first, last := summary.Failures[0], summary.Failures[len(summary.Failures)-1]
	assert.False(t, last.At.Before(first.At), "failures are listed in the order they happened")

	stopping := StartLoad(t, pool, table, LoadSpec{Seed: 3, Workers: 1, RatePerSecond: 50, Mix: Mix{Update: 1}})
	stopDeadline := time.NewTimer(time.Second)
	defer stopDeadline.Stop()
	<-stopDeadline.C
	summary, err = stopping.Stop()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code)
	assert.Empty(t, summary.Failures, "a generator that does not record failures reports the first from Stop instead")
}

// A key move rewrites a row's primary key to the sequence's next value, so
// it writes two keys: the row is gone from the old one and present under
// the new one. Both are touched, the old before the new, so a test that
// excludes the keys written after a mark leaves out both sides of the move.
func TestLoadGeneratorRecordsBothKeysOfAKeyMove(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 200))
	generator := StartLoad(t, pool, table, LoadSpec{Seed: 9, Workers: 2, RatePerSecond: 40, Mix: Mix{KeyMove: 1}})
	runDeadline := time.NewTimer(time.Second)
	defer runDeadline.Stop()
	<-runDeadline.C
	summary, err := generator.Stop()
	require.NoError(t, err)
	require.Positive(t, summary.KeyMoves)
	require.Len(t, summary.TouchedIDs(), 2*summary.KeyMoves, "each key move touches its old and its new key")
	var maxID int64
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT max(id) FROM `+table.Qualified()).Scan(&maxID))
	assert.Contains(t, summary.TouchedIDs(), maxID, "the newest move's new key is the table's highest, and it is touched")
	for i := 0; i < len(summary.Touches); i += 2 {
		assert.Less(t, summary.TouchedIDs()[i], summary.TouchedIDs()[i+1], "a move's old key precedes the new one drawn from the sequence")
	}
}

// A delete-only worker on a two-row table commits exactly twice and then
// finds no row to delete on every later tick: the vanished-row race. Those
// ticks are counted, not committed and not failed, so the summary separates
// what the workload wrote from what it lost to expected races.
func TestLoadGeneratorCountsExpectedRaces(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 2))
	generator := StartLoad(t, pool, table, LoadSpec{Seed: 7, Workers: 1, RatePerSecond: 50, Mix: Mix{Delete: 1}})
	runDeadline := time.NewTimer(time.Second)
	defer runDeadline.Stop()
	<-runDeadline.C
	summary, err := generator.Stop()
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Deletes)
	assert.ElementsMatch(t, []int64{1, 2}, summary.DeletedIDs)
	assert.Zero(t, summary.Inserts+summary.Updates+summary.UniqueMoves)
	assert.Positive(t, summary.Races, "every tick after the table emptied must be counted as a race")
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table.Qualified()).Scan(&count))
	assert.Zero(t, count)
}

// ambiguousDeadline bounds the wait for an update to run out its
// client-side deadline behind the held row locks; one deadline and a few
// ticks suffice.
const ambiguousDeadline = 4 * MutationTimeout

// An update whose row another session holds locked waits past its
// client-side deadline. The client gives up without hearing whether the
// write committed, so the update is not counted or touched; its key is
// listed as ambiguous, one per deadline failure, so a comparison can leave
// the row out instead of judging a write that may have landed after all.
func TestLoadGeneratorListsTheKeyOfAWriteThatRanOutItsDeadline(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 3))
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) })
	_, err = holder.Exec(t.Context(), `SELECT id FROM `+table.Qualified()+` FOR UPDATE`)
	require.NoError(t, err)

	generator := StartLoad(t, pool, table, LoadSpec{Seed: 5, Workers: 1, RatePerSecond: 5, Mix: Mix{Update: 1}, RecordFailures: true})
	require.Eventually(t, func() bool { return len(generator.Snapshot().Ambiguous) > 0 },
		ambiguousDeadline, 50*time.Millisecond, "an update waiting on a held row lock runs out its deadline")
	require.NoError(t, holder.Rollback(t.Context()))
	summary, err := generator.Stop()
	require.NoError(t, err)

	deadlines := 0
	for _, failure := range summary.Failures {
		if errors.Is(failure.Err, context.DeadlineExceeded) {
			deadlines++
		}
	}
	assert.Equal(t, deadlines, len(summary.Ambiguous), "each update that ran out its deadline lists its one key")
	assert.Subset(t, []int64{1, 2, 3}, summary.Ambiguous, "the listed keys are rows the updates picked")
	assert.Len(t, summary.Touches, summary.Updates, "an update counts as touched only when its commit was heard")
}
