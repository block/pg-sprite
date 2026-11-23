package migrate_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedIndexDesired adds a column to a partitioned table and indexes
// it, so the plan runs a native ADD COLUMN and then the partitioned index
// build. The order is what the tests below rely on: the column commits
// first, so a stop that came after it would leave the table half-converged.
const partitionedIndexDesired = `CREATE TABLE events (
	id   int,
	note text
) PARTITION BY RANGE (id);
CREATE INDEX events_note_idx ON events (note);`

// hasNoteColumn reports whether events has the column the plan adds first.
func (d twoDatabases) hasNoteColumn(t *testing.T, schema string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'events' AND column_name = 'note')`, schema).Scan(&exists))
	return exists
}

// Desired-state admission proves the partitioned index build's connection
// before the plan's first statement. With Options.Connection unset, or on
// another database than the pool, the run stops as a failed result carrying
// the plan and no verdict, and the ADD COLUMN planned ahead of the index never
// runs. The control wires the connection to the pool's database and runs the
// same plan to convergence.
func TestRunDesiredStopsBeforeAnyStatementWhenThePartitionedIndexConnectionIsWrong(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createEvents(t)
	desired := parseDesired(t, partitionedIndexDesired)

	t.Run("an unset connection", func(t *testing.T) {
		res, err := migrate.RunDesired(t.Context(), d.pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, runOptions())
		require.ErrorIs(t, err, migrate.ErrConnectionRequired)
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		assert.Empty(t, res.Reason, "a stop is not a refusal")
		assert.Len(t, res.Plan.Statements, 2, "the stop carries the plan it did not run")
		assert.Empty(t, res.Verdicts, "no statement was attempted")
		assert.Contains(t, res.Detail, "nothing was executed")
		assert.False(t, d.hasNoteColumn(t, schema), "the ADD COLUMN planned ahead of the index did not run")
	})

	t.Run("a connection on another database", func(t *testing.T) {
		opts := runOptions()
		opts.Connection = dbconn.Config{URL: d.otherURL}
		res, err := migrate.RunDesired(t.Context(), d.pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, opts)
		mismatch, ok := errors.AsType[*migrate.PartitionedIndexConnectionMismatchError](err)
		require.True(t, ok, "want a PartitionedIndexConnectionMismatchError, got %v", err)
		assert.NotEqual(t, mismatch.PoolDatabase, mismatch.ConnectionDatabase)
		assert.Contains(t, mismatch.Error(), "Options.Connection")
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		assert.Empty(t, res.Verdicts, "no statement was attempted")
		assert.False(t, d.hasNoteColumn(t, schema), "the ADD COLUMN planned ahead of the index did not run")
	})

	t.Run("the control, wired to the pool's database", func(t *testing.T) {
		opts := runOptions()
		opts.Connection = dbconn.Config{URL: d.poolURL}
		res, err := migrate.RunDesired(t.Context(), d.pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, opts)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.True(t, d.hasNoteColumn(t, schema))
	})
}

// The partitioned index build does not honor TABLESPACE, so a desired file
// that places the parent's index in one is refused before anything runs, as
// a refusal and not as an error an orchestrator would retry.
func TestRunDesiredRefusesAPartitionedIndexWithATablespace(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createEvents(t)
	desired := parseDesired(t, `CREATE TABLE events (
	id   int,
	note text
) PARTITION BY RANGE (id);
CREATE INDEX events_note_idx ON events (note) TABLESPACE pg_default;`)
	opts := runOptions()
	opts.Connection = dbconn.Config{URL: d.poolURL}

	res, err := migrate.RunDesired(t.Context(), d.pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, opts)

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, res.Class)
	assert.Contains(t, res.Detail, "nothing was planned or executed")
	assert.False(t, d.hasNoteColumn(t, schema), "the ADD COLUMN planned ahead of the index did not run")
}
