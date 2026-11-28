package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// eventsWithPrimaryKey is partitionedKeyFixture's events table with a primary
// key that includes the partition key, as a desired file declares it.
const eventsWithPrimaryKey = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    code       text,
    CONSTRAINT events_pkey PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);`

// partitionedKeyDesiredFixture is partitionedKeyFixture with the pool and
// URL a desired-state test drives.
func partitionedKeyDesiredFixture(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, url, partitionedKeyFixture(t, pool)
}

// desiredKeyPlan runs diff --json over the desired file and returns its plan.
func desiredKeyPlan(t *testing.T, url, schema, desired string) plan.Report {
	t.Helper()
	cmd := newDiffCmd(t, url, schema, desired)
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report), out.String())
	if report.Disposition == router.DispositionExecute {
		require.NoError(t, err)
	}
	return report
}

// runDesiredKey runs migrate --desired --json with args and returns its
// result.
func runDesiredKey(t *testing.T, url, schema, desired string, args ...string) (migrate.DesiredResult, error) {
	t.Helper()
	var out strings.Builder
	err := desiredCommand(t, url, schema, desired, args...).run(t.Context(), &out)
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result), out.String())
	return result, err
}

// assertPlansTheKeyBuild asserts the plan is the one statement sql, run as
// the partitioned key build: its one step is the statement itself, because
// the build lists the partitions when it runs.
func assertPlansTheKeyBuild(t *testing.T, report plan.Report, sql string) {
	t.Helper()
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	planned := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeAddConstraint, planned.Kind)
	assert.Equal(t, sql, planned.SQL, "the desired file names the table unqualified; the plan qualifies it with the target schema")
	assert.Equal(t, router.DispositionExecute, planned.Disposition)
	assert.Equal(t, planner.ExecutionPartitionedKeyBuild, planned.Execution)
	assert.Equal(t, []string{sql}, planned.ExecSQL)
}

// A primary key a desired file adds to a partitioned table runs as the
// partitioned key build, the route migrate --alter takes for the same
// statement: the planner's concurrent unique build and ADD CONSTRAINT ...
// USING INDEX cannot run on a partitioned parent. diff and the dry run plan
// the build under one fingerprint, the apply gives every partition the key
// and the parent's constraint adopts them, and a rerun finds the table
// converged.
func TestMigrateDesiredAddsAPrimaryKeyToAPartitionedTable(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)

	report := desiredKeyPlan(t, url, schema, eventsWithPrimaryKey)
	assertPlansTheKeyBuild(t, report,
		fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema))

	var dryRunOut strings.Builder
	require.NoError(t, desiredCommand(t, url, schema, eventsWithPrimaryKey, "--dry-run").run(t.Context(), &dryRunOut))
	var dryRun plan.Report
	require.NoError(t, json.Unmarshal([]byte(dryRunOut.String()), &dryRun), dryRunOut.String())
	assert.Equal(t, report.Fingerprint, dryRun.Fingerprint, "the dry run previews the plan diff printed")
	_, found := readKeyAdoption(t, pool, schema, "events_pkey")
	assert.False(t, found, "neither diff nor the dry run adds anything")

	result, err := runDesiredKey(t, url, schema, eventsWithPrimaryKey, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	assert.Equal(t, "converged: all 1 planned statements committed and a fresh plan is empty", result.Detail)
	require.Len(t, result.Verdicts, 1)
	assert.Contains(t, result.Verdicts[0].Detail,
		"2 partitions, 2 built, 0 constrained from an existing index, 0 found already constrained")
	assertKeyOnEveryPartition(t, pool, schema, "events_pkey", "p")

	again, err := runDesiredKey(t, url, schema, eventsWithPrimaryKey)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Equal(t, "already converged: the live table matches the desired schema; nothing to run", again.Detail)
}

// A desired file may leave a unique constraint unnamed. The build needs a
// name, since each partition's index and constraint are named after it, and
// the plan already carries the server's default name, so the constraint runs
// as the build under that name.
func TestMigrateDesiredAddsAnUnnamedUniqueConstraintToAPartitionedTable(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)
	const desired = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    code       text,
    UNIQUE (id, created_at)
) PARTITION BY RANGE (created_at);`

	report := desiredKeyPlan(t, url, schema, desired)
	assertPlansTheKeyBuild(t, report,
		fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_id_created_at_key UNIQUE (id, created_at)`, schema))

	result, err := runDesiredKey(t, url, schema, desired, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	assertKeyOnEveryPartition(t, pool, schema, "events_id_created_at_key", "u")
}

// Duplicate rows in one partition fail its unique build. The desired run
// fails with the build's failed verdict, data-fault with the server's unique
// violation, and its error names the partition; the parent gets no key. Once
// the rows are fixed a rerun resumes: the partition that already has its
// constraint is reused, the failed build's leftover is replaced, and the
// table converges.
func TestMigrateDesiredFailsAPartitionedKeyOnDuplicateRows(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.events (id, created_at, code)
		VALUES (9999, DATE '2026-02-14', 'first'),
		       (9999, DATE '2026-02-14', 'second')`, schema))
	require.NoError(t, err)

	result, err := runDesiredKey(t, url, schema, eventsWithPrimaryKey)
	require.Error(t, err)
	require.NotErrorIs(t, err, verdict.ErrRefused, "the build executed steps, so this is a failure, not a refusal")
	require.ErrorIs(t, err, executor.ErrBuildLeftInvalidIndex)
	assert.ErrorContains(t, err, "partition "+schema+".events_2026_02", "the error names the partition holding the duplicates")
	assert.Equal(t, verdict.OutcomeFailed, result.Outcome)
	require.Len(t, result.Verdicts, 1)
	v := result.Verdicts[0]
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, "23505", v.SQLState, "unique_violation: the rows, not the engine, stopped the run")
	_, found := readKeyAdoption(t, pool, schema, "events_pkey")
	assert.False(t, found, "the parent's key was never added")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		DELETE FROM %s.events
		 WHERE id = 9999
		   AND code = 'second'`, schema))
	require.NoError(t, err)
	resumed, err := runDesiredKey(t, url, schema, eventsWithPrimaryKey)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, resumed.Outcome, resumed.Detail)
	require.Len(t, resumed.Verdicts, 1)
	assert.Contains(t, resumed.Verdicts[0].Detail,
		"2 partitions, 1 built, 0 constrained from an existing index, 1 found already constrained",
		"the January partition's constraint is reused and February's is built again")
	assertKeyOnEveryPartition(t, pool, schema, "events_pkey", "p")
}

// On a plain table the plan adopts a live unique index as the key with ADD
// CONSTRAINT ... USING INDEX, which PostgreSQL refuses on a partitioned
// parent. The partitioned equivalent is a valid unique index of the key's
// definition on each partition, attached to nothing: the desired file models
// only the parent, so the plan is the same key build, and the build gives
// each partition's existing index the constraint instead of building another.
func TestMigrateDesiredAdoptsEachPartitionsUniqueIndexForAPartitionedKey(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)
	for _, partition := range []string{"events_2026_01", "events_2026_02"} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE UNIQUE INDEX CONCURRENTLY %[2]s_id_created_at_uidx
			    ON %[1]s.%[2]s (id, created_at)`, schema, partition))
		require.NoError(t, err)
	}

	report := desiredKeyPlan(t, url, schema, eventsWithPrimaryKey)
	assertPlansTheKeyBuild(t, report,
		fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema))

	result, err := runDesiredKey(t, url, schema, eventsWithPrimaryKey, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	require.Len(t, result.Verdicts, 1)
	assert.Contains(t, result.Verdicts[0].Detail,
		"2 partitions, 0 built, 2 constrained from an existing index, 0 found already constrained")
	assertKeyOnEveryPartition(t, pool, schema, "events_pkey", "p")
	assert.Equal(t, 3, indexCountIn(t, pool, schema), "the parent's index and each partition's existing one; nothing else was built")
}

// A primary key over a column the table allows to be NULL is never handed to
// the build with the column still nullable, where migrate --alter refuses it:
// the desired file's key makes code NOT NULL, so the plan sets it first, with
// the CHECK-backed online sequence, and the build then finds the column NOT
// NULL and runs.
func TestMigrateDesiredSetsNotNullBeforeAPartitionedPrimaryKey(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)
	const desired = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    code       text,
    CONSTRAINT events_pkey PRIMARY KEY (code, created_at)
) PARTITION BY RANGE (created_at);`

	report := desiredKeyPlan(t, url, schema, desired)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 2)
	assert.Equal(t, fmt.Sprintf(`ALTER TABLE %s.events ALTER COLUMN code SET NOT NULL`, schema), report.Statements[0].SQL)
	assert.Equal(t, planner.ExecutionAutocommit, report.Statements[0].Execution)
	assert.Equal(t, planner.ExecutionPartitionedKeyBuild, report.Statements[1].Execution)

	result, err := runDesiredKey(t, url, schema, desired, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	assertKeyOnEveryPartition(t, pool, schema, "events_pkey", "p")
}

// The build cannot carry DEFERRABLE to the partitions' constraints, so a
// deferrable unique constraint a desired file adds is not routed to it and
// keeps the partitioned-parent refusal, in diff and in the run alike.
func TestMigrateDesiredRefusesADeferrableUniqueConstraintOnAPartitionedTable(t *testing.T) {
	pool, url, schema := partitionedKeyDesiredFixture(t)
	const desired = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    code       text,
    CONSTRAINT events_key UNIQUE (id, created_at) DEFERRABLE
) PARTITION BY RANGE (created_at);`

	report := desiredKeyPlan(t, url, schema, desired)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, report.Reason)
	require.Len(t, report.Statements, 1)
	assert.Empty(t, report.Statements[0].Execution, "the statement is not routed to the build")

	result, err := runDesiredKey(t, url, schema, desired)
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, result.Reason)
	assert.Zero(t, indexCountIn(t, pool, schema), "the refusal built no index")
}

// A desired file that adds a column and a primary key to a table with a
// sub-partitioned partition plans the column add ahead of the key build.
// The build refuses such a tree from the catalog alone, so the plan is
// refused whole before anything runs, in the dry run and the run alike: had
// the run reached the build, the column add would already have committed and
// the table could never converge.
func TestMigrateDesiredRefusesAKeyOnASubPartitionedTableBeforeAnythingRuns(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
		    id         bigint NOT NULL,
		    created_at date NOT NULL
		) PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')
		    PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events_2026
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`, schema))
	require.NoError(t, err)
	const desired = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    note       text,
    CONSTRAINT events_pkey PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);`

	report := desiredKeyPlan(t, url, schema, desired)
	require.Len(t, report.Statements, 2)
	assert.Equal(t, schemadiff.ChangeAddColumn, report.Statements[0].Kind, "the column add is planned ahead of the key")
	assert.Equal(t, planner.ExecutionPartitionedKeyBuild, report.Statements[1].Execution)

	dryRun, dryRunErr := runDesiredKey(t, url, schema, desired, "--dry-run")
	require.ErrorIs(t, dryRunErr, verdict.ErrRefused)
	result, err := runDesiredKey(t, url, schema, desired)
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, result.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, result.Class)
	assert.Empty(t, result.Verdicts, "no statement was attempted")
	assert.Equal(t, result, dryRun, "the dry run must report the refusal the run reports")

	var notes int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_attribute
		 WHERE attrelid = format('%I.events', $1::text)::regclass
		   AND attname = 'note'
		   AND NOT attisdropped`, schema).Scan(&notes))
	assert.Zero(t, notes, "the column add planned ahead of the key did not commit")
	assert.Zero(t, indexCountIn(t, pool, schema), "the refusal built no index")
}
