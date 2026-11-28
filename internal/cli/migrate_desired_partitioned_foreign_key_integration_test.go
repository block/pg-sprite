package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// eventsWithAccountForeignKey is partitionedForeignKeyFixture's events table
// with a foreign key to accounts. A desired file names tables unqualified,
// so the reference is to accounts in the target schema.
const eventsWithAccountForeignKey = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
) PARTITION BY RANGE (created_at);`

// desiredPlan runs diff --json over the desired file and returns its plan.
func desiredPlan(t *testing.T, url, schema, desired string) plan.Report {
	t.Helper()
	cmd := newDiffCmd(t, url, schema, desired)
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var report plan.Report
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report), out.String())
	return report
}

// runDesired runs migrate --desired --json with args and returns its result.
func runDesired(t *testing.T, url, schema, desired string, args ...string) (migrate.DesiredResult, error) {
	t.Helper()
	var out strings.Builder
	err := desiredCommand(t, url, schema, desired, args...).run(t.Context(), &out)
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result), out.String())
	return result, err
}

// A foreign key a desired file adds to a partitioned table runs as the
// partitioned foreign key build on PostgreSQL 14 to 17, the route migrate
// --alter takes for the same statement: those versions refuse the NOT VALID
// step of the ordinary route on a partitioned table. The file names accounts
// unqualified; the plan qualifies it with the target schema, which the build
// requires. diff and the dry run plan the build, the apply adopts every
// partition's validated foreign key under the parent's, and a rerun finds the
// table converged.
func TestMigrateDesiredAddsAForeignKeyToAPartitionedTable(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)

	report := desiredPlan(t, url, schema, eventsWithAccountForeignKey)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	planned := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeAddForeignKey, planned.Kind)
	assert.Equal(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)", schema),
		planned.SQL, "the referenced table is qualified with the target schema")
	assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, planned.Execution)
	assert.Equal(t, []string{planned.SQL}, planned.ExecSQL, "the one step is the statement; the build lists the partitions when it runs")

	var dryRunOut strings.Builder
	require.NoError(t, desiredCommand(t, url, schema, eventsWithAccountForeignKey, "--dry-run").run(t.Context(), &dryRunOut))
	var dryRun plan.Report
	require.NoError(t, json.Unmarshal([]byte(dryRunOut.String()), &dryRun), dryRunOut.String())
	assert.Equal(t, report.Fingerprint, dryRun.Fingerprint, "the dry run previews the plan diff printed")
	_, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	assert.False(t, found, "neither diff nor the dry run adds anything")

	result, err := runDesired(t, url, schema, eventsWithAccountForeignKey, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	assert.Equal(t, "converged: all 1 planned statements committed and a fresh plan is empty", result.Detail)
	require.Len(t, result.Verdicts, 1)
	assert.Contains(t, result.Verdicts[0].Detail, "2 partitions, 0 found already validated, 0 validated, 2 added")
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found, "the parent carries the foreign key")
	assert.True(t, fk.validated)
	assert.Equal(t, map[string]bool{
		schema + ".events_2026_01": true,
		schema + ".events_2026_02": true,
	}, fk.adopted, "every partition's validated foreign key is attached to the parent's")

	again, err := runDesired(t, url, schema, eventsWithAccountForeignKey)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Equal(t, "already converged: the live table matches the desired schema; nothing to run", again.Detail)
}

// A desired foreign key with an ON DELETE column list takes the same build on
// PostgreSQL 15 to 17: the plan carries the list, every partition's foreign
// key gets it, and a rerun finds the table converged. PostgreSQL 14 has no
// column list, so the desired file itself is invalid there.
func TestMigrateDesiredAddsAPartitionedForeignKeyWithAnOnDeleteColumnList(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)
	if major < 15 {
		t.Skipf("PostgreSQL %d has no ON DELETE column list", major)
	}
	const desired = `
CREATE TABLE events (
    id         bigint NOT NULL,
    created_at date NOT NULL,
    account_id bigint,
    CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id)
        ON DELETE SET NULL (account_id)
) PARTITION BY RANGE (created_at);`

	report := desiredPlan(t, url, schema, desired)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, fmt.Sprintf(
		"ALTER TABLE %[1]s.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id) ON DELETE SET NULL (account_id)", schema),
		report.Statements[0].SQL)
	assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, report.Statements[0].Execution)

	result, err := runDesired(t, url, schema, desired, "--expect-fingerprint", report.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found)
	assert.Equal(t, map[string]bool{
		schema + ".events_2026_01": true,
		schema + ".events_2026_02": true,
	}, fk.adopted)

	again, err := runDesired(t, url, schema, desired)
	require.NoError(t, err)
	assert.Equal(t, "already converged: the live table matches the desired schema; nothing to run", again.Detail)
}

// A row that references no account breaks the foreign key. The desired run
// fails with the build's failed verdict, which carries the server's error
// naming the partition that holds the row; the parent gets no foreign key,
// and a rerun after the row is fixed resumes and converges.
func TestMigrateDesiredFailsAPartitionedForeignKeyOnAnOrphanRow(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.events (id, created_at, account_id)
		VALUES (9999, DATE '2026-02-14', 424242)`, schema))
	require.NoError(t, err)

	result, err := runDesired(t, url, schema, eventsWithAccountForeignKey)
	require.Error(t, err)
	require.NotErrorIs(t, err, verdict.ErrRefused, "the build executed steps, so this is a failure, not a refusal")
	assert.Equal(t, verdict.OutcomeFailed, result.Outcome)
	require.Len(t, result.Verdicts, 1)
	v := result.Verdicts[0]
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, "23503", v.SQLState, "foreign_key_violation: the rows, not the engine, stopped the run")
	assert.Contains(t, v.ServerError, "events_2026_02", "the server's error names the partition holding the orphan row")
	_, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	assert.False(t, found, "the parent's foreign key was never added")
	assert.Zero(t, partitionForeignKeysAttached(t, pool, schema), "no partition's foreign key was adopted")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.events WHERE id = 9999`, schema))
	require.NoError(t, err)
	resumed, err := runDesired(t, url, schema, eventsWithAccountForeignKey)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, resumed.Outcome, resumed.Detail)
	require.Len(t, resumed.Verdicts, 1)
	assert.Contains(t, resumed.Verdicts[0].Detail, "2 partitions, 1 found already validated, 1 validated, 0 added",
		"the first partition's foreign key is reused, and the second's, added NOT VALID before the row stopped it, is validated")
}

// PostgreSQL 18 accepts NOT VALID on a partitioned table, so a foreign key a
// desired file adds takes the ordinary online route there: ADD ... NOT
// VALID, then VALIDATE CONSTRAINT, not the partitioned build.
func TestMigrateDesiredPartitionedForeignKeyTakesTheOrdinaryRouteOnPostgres18(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	if major < 18 {
		t.Skipf("PostgreSQL %d refuses NOT VALID on a partitioned table, so the partitioned build runs instead", major)
	}

	report := desiredPlan(t, url, schema, eventsWithAccountForeignKey)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.ExecutionAutocommit, report.Statements[0].Execution)
	require.Len(t, report.Statements[0].ExecSQL, 2, "ADD ... NOT VALID, then VALIDATE CONSTRAINT")

	result, err := runDesired(t, url, schema, eventsWithAccountForeignKey)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found)
	assert.True(t, fk.validated)
}
