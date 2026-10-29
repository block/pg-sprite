package planner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A rename cannot share a submission with an operation that reshapes the
// table. In PostgreSQL's grammar `ALTER TABLE ... RENAME COLUMN` is its own
// statement (RenameStmt), not an ALTER TABLE subcommand, and the front door
// admits exactly one statement. So the overlap Spirit guards against on MySQL
// — one ALTER that renames a column and adds or retypes one, leaving the
// copier to map old names to new — cannot reach the planner, and a rename
// can never ride along into copy-and-swap.
func TestClassifyRejectsARenameCombinedWithAnotherOperation(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			// The overlap shape itself: reuse the old name for a new column.
			name: "rename then add a column with the old name",
			sql:  "ALTER TABLE t RENAME COLUMN a TO b, ADD COLUMN a int",
		},
		{
			name: "retype then rename the same column",
			sql:  "ALTER TABLE t ALTER COLUMN a TYPE bigint, RENAME COLUMN a TO b",
		},
		{
			// A volatile default routes copy-and-swap on its own; the rename
			// still cannot join it.
			name: "add a column with a volatile default then rename",
			sql:  "ALTER TABLE t ADD COLUMN c uuid DEFAULT gen_random_uuid(), RENAME COLUMN a TO b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planner.Classify(tc.sql, planner.Facts{})
			require.Error(t, err, "the grammar must refuse to parse this as one statement")
			assert.NotErrorIs(t, err, statement.ErrNotOneStatement, "a syntax error, not a statement count")
		})
	}
}

// Splitting the overlap into two statements does not get it through either:
// the front door takes one statement per submission.
func TestClassifyRejectsARenameAndAnAddSubmittedTogether(t *testing.T) {
	_, err := planner.Classify("ALTER TABLE t RENAME COLUMN a TO b; ALTER TABLE t ADD COLUMN a int", planner.Facts{})
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}

// A rename on its own routes native even when the column is the primary key,
// which is also the replica identity: nothing is copied, so there is no chunk
// key to remap. The route is what a forced rename runs; without --force the
// router refuses the rename as rewrite-required for the application's sake.
// Dropping the same column is refused, which is what the replica-identity
// facts are there to catch.
func TestClassifyRunsAPrimaryKeyRenameNative(t *testing.T) {
	primaryKey := planner.Facts{
		ColumnTypes: map[string]string{"id": "bigint"},
		ReplicaIdentity: schemadiff.ReplicaIdentityKeys{
			Indexes:     []string{"t_pkey"},
			Constraints: []string{"t_pkey"},
			Columns:     []string{"id"},
		},
	}
	plan, err := planner.Classify("ALTER TABLE t RENAME COLUMN id TO user_id", primaryKey)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteNative, plan.Route)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, planner.ReasonAppBreakingRename, plan.Decisions[0].Reason)

	drop, err := planner.Classify("ALTER TABLE t DROP COLUMN id", primaryKey)
	require.NoError(t, err)
	assert.Equal(t, planner.RouteRefuse, drop.Route)
}

// An index rename is metadata-only unless the live fact says the index backs
// a constraint: then the rename renames the constraint, which application SQL
// names, and it is an app-breaking rename. Without the fact (an offline lint)
// it stays metadata-only.
func TestClassifyIndexRenameByTheConstraintItBacks(t *testing.T) {
	const sql = "ALTER INDEX users_handle_key RENAME TO users_username_key"

	plain, err := planner.Classify(sql, planner.Facts{})
	require.NoError(t, err)
	require.Len(t, plain.Decisions, 1)
	assert.Equal(t, planner.ReasonMetadataOnly, plain.Decisions[0].Reason)
	assert.True(t, plain.Decisions[0].ExecutableAsSubmitted())

	backing, err := planner.Classify(sql, planner.Facts{RenamedIndexConstraint: "users_handle_key"})
	require.NoError(t, err)
	require.Len(t, backing.Decisions, 1)
	assert.Equal(t, planner.RouteNative, backing.Route)
	assert.Equal(t, planner.ReasonAppBreakingRename, backing.Decisions[0].Reason)
	assert.False(t, backing.Decisions[0].ExecutableAsSubmitted())
}
