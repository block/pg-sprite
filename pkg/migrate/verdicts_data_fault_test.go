package migrate

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
)

// A failed verdict for a unique build over duplicate rows is data-fault,
// and its detail names the leftover the build left under the requested
// name. The DROP the operator runs once the rows are corrected is offered
// only for a leftover proven this build's own, whether or not the build ran
// as a sequence step, and a committed prefix keeps its own wording with the
// leftover appended.
func TestFailureVerdictNamesTheLeftoverOfADataFaultBuild(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE s.accounts ADD CONSTRAINT k UNIQUE (email)")
	require.NoError(t, err)
	build := &pgconn.PgError{Code: "23505", Message: `could not create unique index "k"`}
	own := &executor.InvalidIndexError{Schema: "s", Index: "k", Table: "accounts", Build: build, Cleanup: executor.ErrBuildLeftInvalidIndex}
	unproven := &executor.InvalidIndexError{Schema: "s", Index: "k", Table: "accounts", Build: build, Cleanup: executor.ErrAbandonmentUnproven}
	const drop = `DROP INDEX CONCURRENTLY "s"."k"`

	t.Run("own leftover, first step", func(t *testing.T) {
		step := &executor.SequenceStepError{Step: 1, Total: 2, Kind: executor.StepConcurrentIndexBuild, SQL: "x", Err: own}
		v := failureVerdict(st, step, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, drop)
		assert.Contains(t, v.Detail, "no earlier steps had committed")
		assert.NotContains(t, v.Detail, "Code names the outcome")
	})
	t.Run("own leftover, no step", func(t *testing.T) {
		v := failureVerdict(st, own, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, drop)
		assert.NotContains(t, v.Detail, "a started bounded attempt rolls back")
	})
	t.Run("committed prefix keeps its wording and appends the leftover", func(t *testing.T) {
		step := &executor.SequenceStepError{Step: 2, Total: 3, Kind: executor.StepConcurrentIndexBuild, SQL: "x", Err: own}
		rep := executor.SequenceReport{Steps: []executor.StepReport{{SQL: "ALTER TABLE s.accounts ADD COLUMN c int", Kind: executor.StepBrief}}}
		v := failureVerdict(st, step, rep, false)
		assert.Contains(t, v.Detail, "the 1 committed steps' state remains")
		assert.Contains(t, v.Detail, drop)
	})
	t.Run("a leftover not proven this run's own offers no drop", func(t *testing.T) {
		step := &executor.SequenceStepError{Step: 1, Total: 2, Kind: executor.StepConcurrentIndexBuild, SQL: "x", Err: unproven}
		v := failureVerdict(st, step, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.NotContains(t, v.Detail, "DROP INDEX CONCURRENTLY")
		assert.Contains(t, v.Detail, "inspect it before any removal")
	})
}

// A column drop refused because a publication now names the column fails
// dropped-object-drift, and the verdict's detail names the publication and
// what clears it, so a reader of the JSON need not parse stderr.
func TestFailureVerdictNamesThePublicationThatRefusedADrop(t *testing.T) {
	st, err := statement.ParseOne("ALTER TABLE s.t DROP COLUMN b")
	require.NoError(t, err)
	published := &executor.PublishedColumnError{Table: `"s"."t"`, Column: "b", Publications: []string{"p524r"}}
	const named = `publication "p524r" now names column b of "s"."t"`

	t.Run("no step", func(t *testing.T) {
		v := failureVerdict(st, published, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeDroppedObjectDrift), v.Code)
		assert.Contains(t, v.Detail, named)
		assert.Contains(t, v.Detail, "take the column out of the publication, then re-plan; nothing was dropped")
	})
	t.Run("sequence step", func(t *testing.T) {
		step := &executor.SequenceStepError{Step: 2, Total: 2, SQL: "x", Err: published}
		v := failureVerdict(st, step, executor.SequenceReport{}, false)
		assert.Equal(t, string(executor.CodeDroppedObjectDrift), v.Code)
		assert.Contains(t, v.Detail, "sequence step 2 of 2 failed")
		assert.Contains(t, v.Detail, named)
	})
}
