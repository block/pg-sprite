package executor_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
)

// A partitioned index build run as step 3 of a caller's 5-step run reports
// every partition as that one step, adds no steps for the partitions it
// finds, and leaves the run's lifecycle to the caller: the tracker is still
// running with the caller's step count once the build returns, and the
// caller's own Finish ends it. The orders table has three partitions, so a
// build that ignored the step would end on step 4 of 8.
func TestBuildPartitionedIndexAsStepReportsEveryPartitionAsTheCallersStep(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		tracker, err := progress.NewTracker(progress.WallClock{})
		require.NoError(t, err)
		tracker.Start(5, progress.OperationAdmitting)

		stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)
		rep, err := executor.BuildPartitionedIndexAsStep(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
			stmt, buildBudget, executor.RunStep{Tracker: tracker, Step: 3})

		require.NoError(t, err)
		require.Len(t, rep.Worked, 3)
		f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		snapshot, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.Equal(t, progress.PhaseRunning, snapshot.Phase, "the caller finishes its run, not the build")
		assert.Equal(t, 3, snapshot.Step, "the last partition reports as the caller's step, not as step 4")
		assert.Equal(t, 5, snapshot.TotalSteps, "the partitions add no steps to the caller's run")
		assert.Contains(t, snapshot.Detail.Statement, "ATTACH PARTITION", "the step's detail names the last partition's attach")

		tracker.Finish(nil)
		snapshot, err = tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.Equal(t, progress.PhaseFinished, snapshot.Phase)
	})
}
