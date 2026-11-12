package progress_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// serverRow is one pg_stat_progress_create_index row as the server
// publishes it for the build's backend.
type serverRow struct {
	phase                   string
	blocksDone, blocksTotal uint64
	tuplesDone, tuplesTotal uint64
	lockersTotal            uint64
	lockersDone             uint64
}

// pollBuild polls a concurrent index build whose server row is row,
// stepElapsed after the step started on a fake clock.
func pollBuild(t *testing.T, row serverRow, stepElapsed time.Duration) progress.Snapshot {
	t.Helper()
	session := fakeSession{query: func(context.Context, string, ...any) pgx.Row {
		return fakeRow{scan: func(dest ...any) error {
			*(dest[0].(*string)) = row.phase
			*(dest[1].(*uint64)) = row.blocksDone
			*(dest[2].(*uint64)) = row.blocksTotal
			*(dest[3].(*uint64)) = row.tuplesDone
			*(dest[4].(*uint64)) = row.tuplesTotal
			*(dest[5].(*uint64)) = row.lockersTotal
			*(dest[6].(*uint64)) = row.lockersDone
			*(dest[7].(**int32)) = nil
			return nil
		}}
	}}
	clock := &fakeClock{now: time.Unix(100, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)
	tracker.Start(1, progress.OperationConcurrentIndex)
	tracker.StartStep(1, progress.OperationConcurrentIndex, "CREATE INDEX CONCURRENTLY idx ON public.t (id)")
	tracker.SetConcurrentBuild(session, 4242)
	clock.now = clock.now.Add(stepElapsed)
	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	return snapshot
}

// Each phase PostgreSQL reports for a btree CREATE INDEX CONCURRENTLY owns
// a band of the whole build, filled by the counter pair the server
// documents for it. A phase that counts nothing sits at its band's start
// even though the server still shows the previous phase's finished
// counters — a sort after a complete heap scan does not jump forward — and
// a wait on lockers or old snapshots does not move with the lockers count,
// whose length other sessions decide.
func TestIndexBuildPercentFollowsThePhaseBands(t *testing.T) {
	cases := []struct {
		name    string
		row     serverRow
		percent float64
	}{
		{"initializing", serverRow{phase: "initializing"}, 0},
		{"waiting for writers before build counts nothing", serverRow{phase: "waiting for writers before build", lockersTotal: 4, lockersDone: 3}, 0},
		{"scanning the table fills 0 to 35 by blocks", serverRow{phase: "building index: scanning table", blocksDone: 20, blocksTotal: 40}, 17.5},
		{"sorting live tuples sits at 35 despite the finished scan's blocks", serverRow{phase: "building index: sorting live tuples", blocksDone: 40, blocksTotal: 40}, 35},
		{"loading tuples fills 35 to 50 by tuples", serverRow{phase: "building index: loading tuples in tree", tuplesDone: 50, tuplesTotal: 100}, 42.5},
		{"waiting for writers before validation sits at 50", serverRow{phase: "waiting for writers before validation", lockersTotal: 2}, 50},
		{"scanning the index fills 50 to 65 by blocks", serverRow{phase: "index validation: scanning index", blocksDone: 10, blocksTotal: 10}, 65},
		{"sorting validation tuples sits at 65", serverRow{phase: "index validation: sorting tuples", blocksDone: 10, blocksTotal: 10}, 65},
		{"validating against the table fills 65 to 95 by blocks", serverRow{phase: "index validation: scanning table", blocksDone: 50, blocksTotal: 100}, 80},
		{"waiting for old snapshots sits at 95", serverRow{phase: "waiting for old snapshots", blocksDone: 100, blocksTotal: 100, lockersTotal: 1}, 95},
		{"an over-reported counter stays inside its band", serverRow{phase: "building index: scanning table", blocksDone: 50, blocksTotal: 40}, 35},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := pollBuild(t, tc.row, time.Second)
			assert.InDelta(t, tc.percent, requirePercent(t, s), 0)
		})
	}
}

// An access method that reports no sub-phase reports the bare "building
// index", and one whose sub-phase the table does not list reads the same
// way: the build band, filled by blocks when the server published a block
// total and by tuples otherwise.
func TestIndexBuildWithoutAKnownSubPhaseUsesTheBuildBand(t *testing.T) {
	byBlocks := pollBuild(t, serverRow{phase: "building index", blocksDone: 30, blocksTotal: 60, tuplesDone: 1, tuplesTotal: 100}, time.Second)
	assert.InDelta(t, 25.0, requirePercent(t, byBlocks), 0, "half the blocks of the 0–50 band")

	byTuples := pollBuild(t, serverRow{phase: "building index: merging posting lists", tuplesDone: 20, tuplesTotal: 100}, time.Second)
	assert.InDelta(t, 10.0, requirePercent(t, byTuples), 0, "a fifth of the tuples of the 0–50 band")
}

// A phase the table does not know — a server version that adds one — has
// no band, so the build reports neither a percent nor an ETA rather than
// guess where it stands.
func TestIndexBuildInAnUnknownPhaseReportsNoPercentOrETA(t *testing.T) {
	s := pollBuild(t, serverRow{phase: "some future phase", blocksDone: 10, blocksTotal: 20}, time.Minute)
	require.NotNil(t, s.Detail.Work, "the counters are still reported")
	assert.Nil(t, s.Detail.Percent)
	assert.Nil(t, s.Detail.ETA)
}

// The build's ETA is projected from its banded percent like any step's:
// 80% after forty seconds leaves ten.
func TestIndexBuildProjectsAnETAFromItsBandedPercent(t *testing.T) {
	s := pollBuild(t, serverRow{phase: "index validation: scanning table", blocksDone: 50, blocksTotal: 100}, 40*time.Second)
	assert.InDelta(t, 80.0, requirePercent(t, s), 0)
	assert.Equal(t, 10*time.Second, requireETA(t, s))
}
