// White-box tests for the desired-state TABLESPACE refusal on a partitioned
// parent's index.

package diffplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
)

// The partitioned index build does not honor TABLESPACE, and the desired
// index's introspected definition drops the clause. So a planned build whose
// desired statement declares a tablespace stops the plan with the build's own
// refusal, read from the statement as written. A named index matches by
// name; an unnamed one cannot be matched, so its tablespace stops any build.
// A tablespace on an index the plan does not build stops nothing.
func TestRefuseDesiredPartitionedIndexTablespace(t *testing.T) {
	const table = "CREATE TABLE events (id bigint, name text) PARTITION BY RANGE (id);\n"
	build := func(sql string) plan.Report {
		return plan.Report{Statements: []plan.Statement{{SQL: sql, Execution: planner.ExecutionPartitionedIndexBuild}}}
	}
	desired := func(t *testing.T, indexes string) statement.DesiredSchema {
		t.Helper()
		ds, err := statement.ParseDesired(table + indexes)
		require.NoError(t, err)
		return ds
	}
	const planned = "CREATE INDEX events_name_idx ON app.events USING btree (name)"

	for _, tc := range []struct{ name, indexes string }{
		{"a named index", "CREATE INDEX events_name_idx ON events (name) TABLESPACE fast;"},
		{"an unnamed index", "CREATE INDEX ON events (name) TABLESPACE fast;"},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			err := refuseDesiredPartitionedIndexTablespace(desired(t, tc.indexes), build(planned))
			require.ErrorIs(t, err, executor.ErrPartitionedIndexTablespace)
			assert.Contains(t, err.Error(), `TABLESPACE "fast"`)
			assert.Contains(t, err.Error(), "events_name_idx")
		})
	}

	t.Run("admits a build beside another index in a tablespace", func(t *testing.T) {
		ds := desired(t, "CREATE INDEX events_id_idx ON events (id) TABLESPACE fast;\n"+
			"CREATE INDEX events_name_idx ON events (name);")
		require.NoError(t, refuseDesiredPartitionedIndexTablespace(ds, build(planned)))
	})

	t.Run("admits a tablespace when no statement runs the build", func(t *testing.T) {
		ds := desired(t, "CREATE INDEX events_name_idx ON events (name) TABLESPACE fast;")
		report := plan.Report{Statements: []plan.Statement{{SQL: planned, Execution: planner.ExecutionAutocommit}}}
		require.NoError(t, refuseDesiredPartitionedIndexTablespace(ds, report))
	})
}
