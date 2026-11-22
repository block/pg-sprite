package executor_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
)

// lockLossDeadline bounds how long a test waits for the build to notice a
// lost table lock: the lock session confirms it on a fixed interval, and the
// build ends when that confirmation fails.
const lockLossDeadline = 30 * time.Second

// A table lock lost while a partition's index is being built cancels that
// build, which leaves an invalid index. The outcome is still the lock's
// loss, an invariant violation that carries dbconn.ErrTableLockLost, and
// never the retryable invalid-index code of the build it cancelled: what the
// build did under an exclusion it no longer holds is not something a rerun
// should assume.
func TestBuildPartitionedIndexReportsALostTableLockAsAnInvariantViolation(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		f.ordersByRegion(t)
		stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)

		release, done := f.stallFirstPartitionBuild(t, stmt)

		lockHolder, found, err := dbconn.LookupTableLockHolder(t.Context(), f.pool, f.schema, "orders")
		require.NoError(t, err)
		require.True(t, found, "the build must hold the table lock")
		var terminated bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, int64(lockHolder.PID)).Scan(&terminated))
		require.True(t, terminated)

		var got buildOutcome
		select {
		case got = <-done:
		case <-time.After(lockLossDeadline):
			release()
			t.Fatalf("the build did not end within %s of its table lock being lost", lockLossDeadline)
		}
		release()

		require.Error(t, got.err)
		assert.ErrorIs(t, got.err, executor.ErrInvariantViolation)
		assert.ErrorIs(t, got.err, dbconn.ErrTableLockLost)
		assert.Equal(t, executor.CodeInvariantViolation, executor.OutcomeCode(got.err))
		assert.Contains(t, got.err.Error(), "LK-1")
		assert.False(t, f.indexesOn(t, "orders")["idx_orders_total"].valid, "the parent's index stays invalid")
	})
}
