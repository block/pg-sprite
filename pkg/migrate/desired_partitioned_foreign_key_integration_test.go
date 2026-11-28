package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// eventsHasNote reports whether schema.events has the note column each
// desired file below adds ahead of its foreign key.
func eventsHasNote(t *testing.T, pool *pgxpool.Pool, schema string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'events' AND column_name = 'note')`, schema).Scan(&exists))
	return exists
}

// assertForeignKeyPlanRefusedWhole runs desired and asserts the run refuses
// the whole plan at admission, before any statement runs: the plan adds the
// note column first and the foreign key second, through the partitioned
// foreign key build, and the build's refusal, read from the catalog at
// admission, keeps the column from committing ahead of it. want is the
// build's sentinel, whose words the detail carries.
func assertForeignKeyPlanRefusedWhole(t *testing.T, pool *pgxpool.Pool, url, schema, desired string, want error) {
	t.Helper()
	res, err := migrate.RunDesired(t.Context(), pool,
		migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desired)}, fkOptions(url))
	require.NoError(t, err, "a refusal returns a nil error")
	require.Len(t, res.Plan.Statements, 2)
	assert.Equal(t, schemadiff.ChangeAddColumn, res.Plan.Statements[0].Kind, "the column is planned ahead of the foreign key")
	assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, res.Plan.Statements[1].Execution)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, res.Class, "the build may learn to take this shape")
	assert.Contains(t, res.Detail, "planned statement 2 (")
	assert.Contains(t, res.Detail, want.Error())
	assert.Contains(t, res.Detail, "nothing was executed")
	assert.Empty(t, res.Verdicts, "no statement was attempted")
	assert.False(t, eventsHasNote(t, pool, schema), "the ADD COLUMN planned ahead of the foreign key did not run")
	assert.Zero(t, foreignKeysIn(t, pool, schema), "no foreign key was added anywhere")
}

// The partitioned foreign key build takes only a plain table as the
// referenced table: a partitioned one makes the parent's statement clone the
// foreign key onto every referenced partition, which the build neither proves
// nor reads back. The desired file adds a column and a foreign key to the
// partitioned ledgers; the run refuses the whole plan before the column is
// added, instead of adding it and refusing the foreign key on every rerun.
func TestRunDesiredRefusesAForeignKeyToAPartitionedTableBeforeAnyStatement(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ledgers (
		    id bigint PRIMARY KEY
		) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.ledgers_low PARTITION OF %[1]s.ledgers
		    FOR VALUES FROM (1) TO (1000)`, schema))
	require.NoError(t, err)

	assertForeignKeyPlanRefusedWhole(t, pool, url, schema, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_ledger_fk FOREIGN KEY (account_id) REFERENCES ledgers (id)
) PARTITION BY RANGE (created_at);`, executor.ErrPartitionedForeignKeyReferencedTable)
}

// A foreign key from a partitioned table to itself references a partitioned
// table, which the build does not take. The desired file adds a column and
// a foreign key from (account_id, created_at) to the table's own primary key;
// the run refuses the whole plan before the column is added.
func TestRunDesiredRefusesASelfReferencingPartitionedForeignKeyBeforeAnyStatement(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema))
	require.NoError(t, err)

	assertForeignKeyPlanRefusedWhole(t, pool, url, schema, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_pkey PRIMARY KEY (id, created_at),
    CONSTRAINT events_parent_fk FOREIGN KEY (account_id, created_at) REFERENCES events (id, created_at)
) PARTITION BY RANGE (created_at);`, executor.ErrPartitionedForeignKeyReferencedTable)
}

// The build gives each leaf the foreign key and adopts them under the
// parent's, so a partition that is itself partitioned is a level it does
// not walk. With a sub-partitioned March partition under events, the
// desired file's column and foreign key to the plain accounts table are
// refused whole before the column is added.
func TestRunDesiredRefusesAPartitionedForeignKeyOnASubPartitionedTreeBeforeAnyStatement(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')
		    PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_2026_03_low PARTITION OF %[1]s.events_2026_03
		    FOR VALUES FROM (1) TO (1000)`, schema))
	require.NoError(t, err)

	assertForeignKeyPlanRefusedWhole(t, pool, url, schema, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
) PARTITION BY RANGE (created_at);`, executor.ErrPartitionedForeignKeySubPartitioned)
}

// A foreign table cannot carry a foreign key, so the build fails on a tree
// that holds one, as a failed verdict rather than a refusal. Admission reads
// the tree before any statement runs, so the run stops whole as a failed
// result carrying the plan and no verdict, its error the build's, and the
// column planned ahead of the foreign key is not added.
func TestRunDesiredStopsBeforeAnyStatementOnAForeignTablePartition(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	wrapper := pgx.Identifier{schema + "_fdw"}.Sanitize()
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP FOREIGN DATA WRAPPER IF EXISTS "+wrapper+" CASCADE")
		assert.NoError(t, err, "drop the test's foreign-data wrapper")
	})
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FOREIGN DATA WRAPPER %[2]s;
		CREATE SERVER %[2]s FOREIGN DATA WRAPPER %[2]s;
		CREATE FOREIGN TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')
		    SERVER %[2]s`, schema, wrapper))
	require.NoError(t, err)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
) PARTITION BY RANGE (created_at);`)}, fkOptions(url))
	require.ErrorIs(t, err, executor.ErrForeignTablePartition)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Reason, "a stop is not a refusal")
	require.Len(t, res.Plan.Statements, 2, "the stop carries the plan it did not run")
	assert.Contains(t, res.Detail, "nothing was executed")
	assert.Empty(t, res.Verdicts, "no statement was attempted")
	assert.False(t, eventsHasNote(t, pool, schema), "the ADD COLUMN planned ahead of the foreign key did not run")
}

// A partition whose DETACH PARTITION ... CONCURRENTLY did not finish is left
// out when the server attaches the parent's foreign key, so the build fails
// on the tree, as it does on a foreign table. With events_2026_02 left
// detach-pending, admission stops the run whole before any statement: a
// failed result carrying the plan and no verdict, its error the build's, and
// the column planned ahead of the foreign key is not added.
func TestRunDesiredStopsBeforeAnyStatementOnADetachPendingPartition(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	leaveDetachPending(t, pool, schema, "events_2026_02")

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
) PARTITION BY RANGE (created_at);`)}, fkOptions(url))
	require.ErrorIs(t, err, executor.ErrPartitionDetachPending)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Reason, "a stop is not a refusal")
	require.Len(t, res.Plan.Statements, 2, "the stop carries the plan it did not run")
	assert.Contains(t, res.Detail, "nothing was executed")
	assert.Empty(t, res.Verdicts, "no statement was attempted")
	assert.False(t, eventsHasNote(t, pool, schema), "the ADD COLUMN planned ahead of the foreign key did not run")
}

// The control: the same column and foreign key to the plain accounts table on
// a single-level tree pass admission, and the run adds both.
func TestRunDesiredAdmitsAPartitionedForeignKeyTheBuildTakes(t *testing.T) {
	pool, url, schema := partitionedEvents(t)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    note       text,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
) PARTITION BY RANGE (created_at);`)}, fkOptions(url))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	require.Len(t, res.Verdicts, 2)
	assert.True(t, eventsHasNote(t, pool, schema))
	assert.Equal(t, 3, foreignKeysIn(t, pool, schema), "the parent's foreign key and each of the two partitions' it adopted")
}
