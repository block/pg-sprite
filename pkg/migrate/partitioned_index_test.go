package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A partitioned index build that stops reports a failed verdict whose detail
// never claims what the error cannot prove. A name collision can come before
// anything is created, and a foreign-table partition can be found on a later
// catalog read after indexes committed, so every stop says earlier steps may
// have committed and tells the operator to inspect and rerun, whichever error
// stopped it. A permanent cause also tells the operator to fix it first,
// since a rerun fails the same way until then. An unknown commit outcome says so in its own words.
func TestPartitionedIndexFailureVerdictClaimsOnlyWhatTheErrorProves(t *testing.T) {
	st, err := statement.ParseOne("CREATE INDEX orders_total_idx ON billing.orders (total)")
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"a name collision", executor.ErrPartitionedIndexNameTaken, true},
		{"a foreign-table partition", executor.ErrForeignTablePartition, true},
		{"a table that is not partitioned", executor.ErrNotPartitionedTable, true},
		{"a pool that is too small", executor.ErrPoolTooSmall, true},
		{"a lock session the pool cannot see", executor.ErrTableLockUnconfirmed, true},
		{"a budget that ran out", &executor.BudgetError{Cause: executor.CauseLock}, false},
		{"a wrapped database error", fmt.Errorf("read the partitions: %w", errors.New("connection reset")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := partitionedIndexFailureVerdict(st, tc.err)
			assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
			assert.Equal(t, "billing.orders", v.Table)
			assert.Contains(t, v.Detail, "steps before the stop may have committed")
			assert.Contains(t, v.Detail, "inspect the indexes of billing.orders and its partitions")
			assert.Contains(t, v.Detail, "run the same statement again")
			assert.NotContains(t, v.Detail, "before it created anything", "the error does not prove nothing was created")
			assert.NotContains(t, v.Detail, "remain,", "the error does not prove what remains")
			if tc.permanent {
				assert.Contains(t, v.Detail, "fix the cause the error names", "a permanent cause fails a rerun the same way")
				assert.Contains(t, v.Detail, "("+v.Code+")")
			} else {
				assert.NotContains(t, v.Detail, "fix the cause")
			}
		})
	}

	t.Run("an unknown commit outcome", func(t *testing.T) {
		v := partitionedIndexFailureVerdict(st, &executor.CommitOutcomeUnknownError{})
		assert.Contains(t, v.Detail, "outcome unknown: a COMMIT did not return success")
		assert.Contains(t, v.Detail, "inspect the indexes of billing.orders and its partitions")
	})
}
