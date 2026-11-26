package migrate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

// A partition whose rows break a unique index stops the build with a
// data-fault verdict that names the partition and the invalid index the
// failed build left on it, and that a rerun meets the same rows until they
// are corrected. An invalid index the run could not prove its own is placed
// on no table and gets the investigation steps the plain build gives.
func TestPartitionedIndexFailureVerdictNamesThePartitionOfADataFault(t *testing.T) {
	st, err := statement.ParseOne("CREATE UNIQUE INDEX orders_id_uq ON billing.orders (id, placed_on)")
	require.NoError(t, err)
	build := &pgconn.PgError{Code: "23505", Message: `could not create unique index "orders_id_uq_p16397"`}
	failed := func(cleanup error) error {
		return fmt.Errorf("build the index of partition billing.orders_2026_02: %w", &executor.InvalidIndexError{
			Schema: "billing", Index: "orders_id_uq_p16397", Table: "orders_2026_02", Build: build, Cleanup: cleanup,
		})
	}

	t.Run("own leftover", func(t *testing.T) {
		v := partitionedIndexFailureVerdict(st, failed(executor.ErrBuildLeftInvalidIndex))
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, `the rows of partition "billing"."orders_2026_02" violate index "billing"."orders_id_uq_p16397"`)
		assert.Contains(t, v.Detail, "every rerun meets the same rows until they are corrected")
		assert.Contains(t, v.Detail, "correct the rows, then run the same statement again")
		assert.Contains(t, v.Detail, `"billing"."orders_id_uq_p16397" invalid on "billing"."orders_2026_02"`)
		assert.Contains(t, v.Detail, "the partitions after it not yet reached by this run")
		assert.NotContains(t, v.Detail, "fix the cause the error names")
	})
	t.Run("a leftover not proven this run's own", func(t *testing.T) {
		v := partitionedIndexFailureVerdict(st, failed(executor.ErrAbandonmentUnproven))
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, `violate index "billing"."orders_id_uq_p16397"`)
		assert.Contains(t, v.Detail, "inspect it before any removal")
		assert.NotContains(t, v.Detail, "orders_2026_02", "the leftover is not placed on a table the run did not prove")
		assert.NotContains(t, v.Detail, "run the same statement again")
	})
}

// PostgreSQL lets a partition live in another schema than its parent. The
// detail names the partition and its index by the partition's own schema,
// which the failed build's error carries, never by the parent's.
func TestPartitionedIndexFailureVerdictNamesAPartitionInAnotherSchema(t *testing.T) {
	st, err := statement.ParseOne("CREATE UNIQUE INDEX orders_id_uq ON billing.orders (id, placed_on)")
	require.NoError(t, err)
	v := partitionedIndexFailureVerdict(st, fmt.Errorf("build the index of partition archive.orders_2026_02: %w", &executor.InvalidIndexError{
		Schema: "archive", Index: "orders_id_uq_p16397", Table: "orders_2026_02",
		Build:   &pgconn.PgError{Code: "23505", Message: `could not create unique index "orders_id_uq_p16397"`},
		Cleanup: executor.ErrBuildLeftInvalidIndex,
	}))
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, "billing.orders", v.Table, "the verdict's table is the parent the statement named")
	assert.Contains(t, v.Detail, `the rows of partition "archive"."orders_2026_02" violate index "archive"."orders_id_uq_p16397"`)
	assert.Contains(t, v.Detail, `"archive"."orders_id_uq_p16397" invalid on "archive"."orders_2026_02"`)
}

// A rerun whose recovery of a partition's invalid index meets another
// backend still building it fails with an *InvalidIndexError that names the
// partition, but its code is invalid-index-build-in-flight, not data-fault:
// the rows are not at fault, so the data-fault detail does not apply.
func TestPartitionedIndexDataFaultDetailLeavesAnInFlightBuildAlone(t *testing.T) {
	err := fmt.Errorf("recover the invalid index billing.orders_id_uq_p16397 on partition billing.orders_2026_02: %w", &executor.InvalidIndexError{
		Schema: "billing", Index: "orders_id_uq_p16397", Table: "orders_2026_02", BuilderPID: 4242,
		Cleanup: executor.ErrInvalidIndexBuildInFlight,
	})
	_, dataFault := partitionedIndexDataFaultDetail("billing.orders", err)
	assert.False(t, dataFault)
	st, parseErr := statement.ParseOne("CREATE UNIQUE INDEX orders_id_uq ON billing.orders (id, placed_on)")
	require.NoError(t, parseErr)
	assert.Equal(t, string(executor.CodeInvalidIndexBuildInFlight), partitionedIndexFailureVerdict(st, err).Code)
}
