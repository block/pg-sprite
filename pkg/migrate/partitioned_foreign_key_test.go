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

const eventsAccountFK = `
	ALTER TABLE app.events
	    ADD CONSTRAINT events_account_fk
	    FOREIGN KEY (account_id) REFERENCES app.accounts (id)`

// A stopped partitioned foreign key build is a refusal only when nothing was
// executed. The shape refusals come before the build touches anything, so
// they always are. The parent, referenced table, and sub-partition checks
// are refusals while no partition has the foreign key yet, and failed
// verdicts once one has, since the build reads the tree again on a later
// pass. Every other stop, such as a name an occupant holds on a partition or
// rows that break the foreign key, is a failed verdict carrying its code.
func TestPartitionedForeignKeyRefusalVerdictRefusesOnlyWhatRanNothing(t *testing.T) {
	st, err := statement.ParseOne(eventsAccountFK)
	require.NoError(t, err)
	none := executor.PartitionedForeignKeyReport{}
	oneLeaf := executor.PartitionedForeignKeyReport{Leaves: []executor.PartitionForeignKeyResult{
		{Table: "events_2026_01", Action: executor.PartitionForeignKeyAdded},
	}}

	for _, tc := range []struct {
		name  string
		err   error
		rep   executor.PartitionedForeignKeyReport
		class verdict.Class
	}{
		{"an unnamed foreign key", fmt.Errorf("%w: no name", executor.ErrPartitionedForeignKeyUnsupportedShape), oneLeaf, verdict.ClassCapabilityBoundary},
		{"not a foreign key", executor.ErrNotAddForeignKey, none, verdict.ClassCapabilityBoundary},
		{"a parent conflict before any partition", fmt.Errorf("app.events: %w", executor.ErrPartitionedForeignKeyParentConflict), none, verdict.ClassEnvironmental},
		{"a partitioned referenced table before any partition", fmt.Errorf("app.accounts: %w", executor.ErrPartitionedForeignKeyReferencedTable), none, verdict.ClassCapabilityBoundary},
		{"a sub-partition before any partition", fmt.Errorf("app.events: %w", executor.ErrPartitionedForeignKeySubPartitioned), none, verdict.ClassCapabilityBoundary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, refused := partitionedForeignKeyRefusalVerdict(st, tc.err, tc.rep)
			require.True(t, refused)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, tc.class, v.Class)
			assert.Equal(t, "app.events", v.Table)
			assert.Contains(t, v.Detail, "nothing was executed")
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		rep  executor.PartitionedForeignKeyReport
	}{
		{"a sub-partition after a partition got its foreign key", executor.ErrPartitionedForeignKeySubPartitioned, oneLeaf},
		{"a referenced table after a partition got its foreign key", executor.ErrPartitionedForeignKeyReferencedTable, oneLeaf},
		{"a parent conflict after a partition got its foreign key", executor.ErrPartitionedForeignKeyParentConflict, oneLeaf},
		{"a name taken on a partition", executor.ErrPartitionedForeignKeyNameTaken, none},
		{"a conflicting partition foreign key", executor.ErrPartitionedForeignKeyConflict, none},
		{"a foreign table partition", executor.ErrForeignTablePartition, none},
		{"a pending detach", executor.ErrPartitionDetachPending, none},
		{"partitions that kept appearing", executor.ErrPartitionedForeignKeyUnsettled, oneLeaf},
		{"a server that takes NOT VALID", executor.ErrPartitionedForeignKeyNotValidSupported, none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refused := partitionedForeignKeyRefusalVerdict(st, tc.err, tc.rep)
			assert.False(t, refused)
		})
	}
}

// The failed verdict carries the executor's code and never claims what the
// error cannot prove: earlier steps may have committed, so it says how to
// find out and that the same statement resumes.
func TestPartitionedForeignKeyFailureVerdictCarriesTheCode(t *testing.T) {
	st, err := statement.ParseOne(eventsAccountFK)
	require.NoError(t, err)

	for err, code := range map[error]executor.Code{
		fmt.Errorf("app.events_2026_01: %w", executor.ErrPartitionedForeignKeyNameTaken): executor.CodePartitionedForeignKeyNameTaken,
		fmt.Errorf("app.events_2026_01: %w", executor.ErrPartitionedForeignKeyConflict):  executor.CodePartitionedForeignKeyConflict,
		fmt.Errorf("app.events: %w", executor.ErrPartitionedForeignKeyUnsettled):         executor.CodePartitionedForeignKeyUnsettled,
		fmt.Errorf("app.events: %w", executor.ErrPartitionedForeignKeySubPartitioned):    executor.CodePartitionedForeignKeySubPartitioned,
	} {
		v := partitionedForeignKeyFailureVerdict(st, err)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(code), v.Code)
		assert.Contains(t, v.Detail, "steps before the stop may have committed")
		assert.Contains(t, v.Detail, "inspect the foreign keys of app.events and its partitions")
		assert.Contains(t, v.Detail, "run the same statement again")
		if code.Permanent() {
			assert.Contains(t, v.Detail, "a cause that a rerun does not clear", string(code))
		} else {
			assert.NotContains(t, v.Detail, "a cause that a rerun does not clear", string(code))
		}
	}

	// Permanent for the build, yet a rerun through Run clears it: on
	// PostgreSQL 18 the statement is not routed to the build at all.
	notValid := partitionedForeignKeyFailureVerdict(st, fmt.Errorf("app.events: %w", executor.ErrPartitionedForeignKeyNotValidSupported))
	assert.Equal(t, string(executor.CodePartitionedForeignKeyNotValidSupported), notValid.Code)
	assert.NotContains(t, notValid.Detail, "a cause that a rerun does not clear")
	assert.Contains(t, notValid.Detail, "takes the ordinary NOT VALID then VALIDATE route on app.events")

	unknown := partitionedForeignKeyFailureVerdict(st, &executor.CommitOutcomeUnknownError{})
	assert.Contains(t, unknown.Detail, "outcome unknown: a COMMIT did not return success")
}

// The detail of a finished build names how many partitions it found
// already validated, validated, and added, and that the parent's ADD
// adopted them; a rerun after the parent's foreign key exists says nothing
// was changed.
func TestPartitionedForeignKeyDetail(t *testing.T) {
	added := partitionedForeignKeyDetail(executor.PartitionedForeignKeyReport{
		Constraint: "events_account_fk", ParentAdded: true, Partitions: 3,
		Leaves: []executor.PartitionForeignKeyResult{
			{Table: "events_2026_01", Action: executor.PartitionForeignKeyFound},
			{Table: "events_2026_02", Action: executor.PartitionForeignKeyValidated},
			{Table: "events_2026_03", Action: executor.PartitionForeignKeyAdded},
		},
	})
	assert.Contains(t, added, "the parent's ADD CONSTRAINT events_account_fk adopted them")
	assert.Contains(t, added, "ACCESS EXCLUSIVE on the referenced table")
	assert.Contains(t, added, "3 partitions, 1 found already validated, 1 validated, 1 added")

	done := partitionedForeignKeyDetail(executor.PartitionedForeignKeyReport{Constraint: "events_account_fk", Partitions: 3})
	assert.Equal(t, "the table is partitioned and already had foreign key events_account_fk, adopted on all 3 partitions; nothing was changed", done)
}
