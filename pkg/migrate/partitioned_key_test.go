package migrate

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A stopped partitioned key build is a refusal only when nothing was
// executed. The shape refusals and the partition key check come before the
// build touches anything, so they always are. The parent, nullable column,
// and sub-partition checks are refusals while no partition has the key yet,
// and failed verdicts once one has, since the build reads them again on a
// later pass. Every other stop, such as a name an occupant holds on a
// partition, is a failed verdict carrying its code.
func TestPartitionedKeyRefusalVerdictRefusesOnlyWhatRanNothing(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE billing.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)")
	require.NoError(t, err)
	none := executor.PartitionedKeyReport{}
	oneLeaf := executor.PartitionedKeyReport{Leaves: []executor.PartitionKeyResult{{Table: "orders_2026", Action: executor.PartitionKeyBuilt}}}

	for _, tc := range []struct {
		name  string
		err   error
		rep   executor.PartitionedKeyReport
		class verdict.Class
	}{
		{"an unnamed key", fmt.Errorf("%w: no name", executor.ErrPartitionedKeyUnsupportedShape), oneLeaf, verdict.ClassCapabilityBoundary},
		{"not a key", executor.ErrNotAddKeyConstraint, none, verdict.ClassCapabilityBoundary},
		{"a key missing a partition key column", &executor.UniqueIndexPartitionKeyError{Index: "billing.orders_pkey"}, none, verdict.ClassByDesign},
		{"a parent conflict before any partition", fmt.Errorf("billing.orders: %w", executor.ErrPartitionedKeyParentConflict), none, verdict.ClassEnvironmental},
		{"a nullable column before any partition", fmt.Errorf("billing.orders: %w", executor.ErrPartitionedKeyNullableColumn), none, verdict.ClassEnvironmental},
		{"a sub-partition before any partition", fmt.Errorf("billing.orders: %w", executor.ErrPartitionedKeySubPartitioned), none, verdict.ClassCapabilityBoundary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, refused := partitionedKeyRefusalVerdict(st, tc.err, tc.rep)
			require.True(t, refused)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, tc.class, v.Class)
			assert.Equal(t, "billing.orders", v.Table)
			assert.Contains(t, v.Detail, "nothing was executed")
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		rep  executor.PartitionedKeyReport
	}{
		{"a sub-partition after a partition got its key", executor.ErrPartitionedKeySubPartitioned, oneLeaf},
		{"a parent conflict after a partition got its key", executor.ErrPartitionedKeyParentConflict, oneLeaf},
		{"a name taken on a partition", executor.ErrPartitionedKeyNameTaken, none},
		{"a conflicting partition key", executor.ErrPartitionedKeyConflict, none},
		{"a foreign table partition", executor.ErrForeignTablePartition, none},
		{"partitions that kept appearing", executor.ErrPartitionedKeyUnsettled, oneLeaf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refused := partitionedKeyRefusalVerdict(st, tc.err, tc.rep)
			assert.False(t, refused)
		})
	}
}

// The failed verdict carries the executor's code and never claims what the
// error cannot prove: earlier steps may have committed, so it says how to
// find out and that the same statement resumes.
func TestPartitionedKeyFailureVerdictCarriesTheCode(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE billing.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)")
	require.NoError(t, err)

	for err, code := range map[error]executor.Code{
		fmt.Errorf("billing.orders_2026: %w", executor.ErrPartitionedKeyNameTaken): executor.CodePartitionedKeyNameTaken,
		fmt.Errorf("billing.orders_2026: %w", executor.ErrPartitionedKeyConflict):  executor.CodePartitionedKeyConflict,
		fmt.Errorf("billing.orders: %w", executor.ErrPartitionedKeyUnsettled):      executor.CodePartitionedKeyUnsettled,
		fmt.Errorf("billing.orders: %w", executor.ErrPartitionedKeySubPartitioned): executor.CodePartitionedKeySubPartitioned,
	} {
		v := partitionedKeyFailureVerdict(st, err)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(code), v.Code)
		assert.Contains(t, v.Detail, "steps before the stop may have committed")
		assert.Contains(t, v.Detail, "inspect the constraints and indexes of billing.orders and its partitions")
		assert.Contains(t, v.Detail, "run the same statement again")
	}

	unknown := partitionedKeyFailureVerdict(st, &executor.CommitOutcomeUnknownError{})
	assert.Contains(t, unknown.Detail, "outcome unknown: a COMMIT did not return success")
}
