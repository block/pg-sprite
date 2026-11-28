package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The build's catalog stops split as its verdicts do: a referenced table it
// does not take and a sub-partitioned partition are refusals, whose detail
// names the tables, since a refusal returns no error to carry them; a
// foreign table partition, a pending detach, and anything else are failures,
// whose detail is a fixed sentence and never the wrapped error's text.
func TestPartitionedForeignKeyStopsSplitAndSayWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		refused bool
		detail  string
	}{
		"referenced table": {executor.ErrPartitionedForeignKeyReferencedTable, true, "app.events_01 under app.events; reference a plain table outside the partition tree"},
		"sub-partitioned":  {executor.ErrPartitionedForeignKeySubPartitioned, true, "app.events_01 under app.events"},
		"foreign table":    {executor.ErrForeignTablePartition, false, "detach the foreign table, then run the plan again"},
		"detach pending":   {executor.ErrPartitionDetachPending, false, "run that statement again to finish the detach"},
		"catalog read":     {errors.New("read the partition tree: connection reset by peer"), false, "could not be checked"},
	} {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("%w: app.events_01 under app.events", tc.err)
			_, refused := partitionedForeignKeyCatalogRefusal(err)
			assert.Equal(t, tc.refused, refused)
			reason := partitionedForeignKeyStopReason(err)
			assert.Contains(t, reason, tc.detail)
			assert.NotContains(t, reason, "index", "the index build's words")
			assert.NotContains(t, reason, "connection reset", "no raw error")
		})
	}
}

// A plan routed to the build whose statement names an unqualified table
// stops before any catalog read, so it needs no pool. It is a failure, not
// a refusal, its detail does not borrow the index build's sentence, and its
// error carries the cause for the host.
func TestAdmitPlanPartitionedForeignKeysStopsAnUnqualifiedTableInItsOwnWords(t *testing.T) {
	sql := `ALTER TABLE events
		ADD CONSTRAINT events_account_fk
		FOREIGN KEY (account_id) REFERENCES app.accounts (id)`
	res, admitted, err := AdmitPlanPartitionedForeignKeys(t.Context(), nil, plan.Report{
		Statements: []plan.Statement{{SQL: sql, ExecSQL: []string{sql}, Execution: planner.ExecutionPartitionedForeignKeyBuild}},
	})
	require.Error(t, err)
	assert.False(t, admitted)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.NotContains(t, res.Detail, "concurrent index build")
	assert.Contains(t, res.Detail, "nothing was executed")
}
