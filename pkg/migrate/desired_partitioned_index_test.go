package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Partitioned index admission reads the database only for a plan with a
// partitioned index build: a plan without one is admitted with no
// connection at all, and a plan with one whose caller never wired
// Options.Connection stops before any read with the sentinel, so a library
// caller can tell its own missing wiring from a fact about the table.
func TestAdmitPartitionedIndexBuild(t *testing.T) {
	t.Run("a plan without the build is admitted without a read", func(t *testing.T) {
		report := plan.Report{Schema: "billing", Table: "orders", Statements: []plan.Statement{
			{SQL: "ALTER TABLE billing.orders ADD COLUMN note text", Backend: router.BackendNative},
		}}
		stopped, admitted, err := admitPartitionedIndexBuild(t.Context(), nil, report, Options{})
		require.NoError(t, err)
		assert.True(t, admitted)
		assert.Zero(t, stopped)
	})

	t.Run("a plan with the build and no connection stops with the sentinel", func(t *testing.T) {
		report := plan.Report{Schema: "billing", Table: "orders", Statements: []plan.Statement{
			{SQL: "ALTER TABLE billing.orders ADD COLUMN note text", Backend: router.BackendNative},
			{
				SQL:       "CREATE INDEX orders_note_idx ON billing.orders (note)",
				Backend:   router.BackendNative,
				Execution: planner.ExecutionPartitionedIndexBuild,
			},
		}}
		stopped, admitted, err := admitPartitionedIndexBuild(t.Context(), nil, report, Options{})
		require.ErrorIs(t, err, ErrConnectionRequired)
		assert.False(t, admitted)
		assert.Equal(t, verdict.OutcomeFailed, stopped.Outcome)
		assert.Empty(t, stopped.Verdicts, "no statement was attempted")
		assert.Equal(t, report, stopped.Plan)
		assert.Contains(t, stopped.Detail, "planned statement 2 of 2")
		assert.Contains(t, stopped.Detail, "Options.Connection is not configured")
		assert.Contains(t, stopped.Detail, "nothing was executed")
	})
}
