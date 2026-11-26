package migrate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// eventsWithoutKindIndex is the desired file for app.events once its
// events_kind_idx is removed. The partitions are not part of the file.
const eventsWithoutKindIndex = `CREATE TABLE events (
    id bigint NOT NULL,
    kind text NOT NULL,
    created_at timestamp with time zone NOT NULL
) PARTITION BY RANGE (created_at);`

// seedEventsWithKindIndex creates a range-partitioned events table with
// three monthly partitions and an index on the parent. PostgreSQL makes
// events_kind_idx a partitioned index and builds one index on each
// partition, attached under it: four indexes in all.
func seedEventsWithKindIndex(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			kind text NOT NULL,
			created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');

		CREATE INDEX events_kind_idx ON %[1]s.events (kind);`, schema))
	require.NoError(t, err)
	require.Equal(t, 4, kindIndexCount(t, pool, schema), "the parent's index and one per partition")
	return pool, schema
}

// kindIndexCount counts the indexes in the schema: the parent's
// events_kind_idx and each partition's index attached under it.
func kindIndexCount(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1`, schema).Scan(&n))
	return n
}

// blockingDropOptions is a run policy whose brief budget the blocking drop
// runs under: a short lock budget and a statement budget longer than it.
func blockingDropOptions() migrate.Options {
	opts := runOptions()
	opts.Budget.Brief = executor.Budget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 10 * time.Second}
	return opts
}

// reviewKindIndexDrop derives the plan an operator reviews for the desired
// file and checks it carries the drop as the refused blocking drop, with the
// plain DROP INDEX as its one step.
func reviewKindIndexDrop(t *testing.T, pool *pgxpool.Pool, schema string) plan.Report {
	t.Helper()
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, eventsWithoutKindIndex)})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	ps := report.Statements[0]
	drop := fmt.Sprintf("DROP INDEX %s.events_kind_idx", schema)
	require.Equal(t, drop, ps.SQL)
	require.Equal(t, router.DispositionRefuse, ps.Disposition)
	require.Equal(t, []string{drop}, ps.ExecSQL)
	require.Equal(t, planner.ExecutionBlockingIndexDrop, ps.Execution)
	return report
}

// The reviewed plan runs the drop PostgreSQL accepts for a partitioned
// table's index when the run pins it, names the table in the destructive
// acknowledgement, and accepts the table's blocking lock: the plain DROP
// INDEX removes the parent's index and every partition's index, the verdict
// keeps the refusal it lifted and the budgets it ran under, and the run is
// executed without online safety.
func TestRunDesiredDropsPartitionedIndexUnderAcceptedBlocking(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
		AcceptBlocking:      table,
	}, blockingDropOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, res.Outcome, res.Detail)
	require.NotNil(t, res.Convergence)
	assert.Equal(t, migrate.ConvergenceVerified, res.Convergence.Status)
	require.Len(t, res.Verdicts, 1)
	v := res.Verdicts[0]
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Equal(t, verdict.CauseParentConcurrentIndexDrop, v.Cause)
	assert.True(t, v.BlockingPassthrough)
	assert.True(t, v.AllowedDestructive)
	assert.Equal(t, table, v.Table)
	assert.Equal(t, []string{fmt.Sprintf("DROP INDEX %s.events_kind_idx", schema)}, v.ExecutedSQL)
	assert.Equal(t, "200ms", v.LockTimeout)
	assert.Equal(t, "10s", v.StatementTimeout)
	assert.Contains(t, v.Detail, table+", every partition of it, and every partition's index were locked")
	assert.Zero(t, kindIndexCount(t, pool, schema), "the parent's index and every partition's index are gone")
}

// An index over a function in the table's schema runs too: the executor
// reads the index again under the search_path the plan read it under, the
// schema then public, so the function prints unqualified in both reads and
// the unchanged index is not mistaken for drift.
func TestRunDesiredBlockingDropOfAnIndexOverASchemaFunction(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.events_kind_idx;

		CREATE FUNCTION %[1]s.norm(text) RETURNS text
			LANGUAGE sql IMMUTABLE
			AS $$ SELECT lower($1) $$;

		CREATE INDEX events_kind_idx ON %[1]s.events (%[1]s.norm(kind));`, schema))
	require.NoError(t, err)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
		AcceptBlocking:      table,
	}, blockingDropOptions())
	require.NoError(t, err, res.Detail)
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, res.Outcome)
	assert.Zero(t, kindIndexCount(t, pool, schema), "the parent's index and every partition's index are gone")
}

// Without the acceptance the plan is refused as it always was, whole and
// before anything runs, and the refusal names the flags that run the drop.
func TestRunDesiredRefusesPartitionedIndexDropWithoutAcceptance(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
	}, blockingDropOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
	assert.Equal(t, verdict.ClassByDesign, res.Class)
	assert.Empty(t, res.Verdicts)
	assert.Contains(t, res.Detail, "--accept-blocking "+table)
	assert.Equal(t, 4, kindIndexCount(t, pool, schema), "nothing was dropped")
}

// A session reading one partition holds ACCESS SHARE on it, which the
// parent's ACCESS EXCLUSIVE request reaches. The request waits out the
// lock budget, not the statement budget, and the run is refused with
// nothing dropped.
func TestRunDesiredBlockingIndexDropRefusesAnExhaustedLockBudget(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"

	reader, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.events_2026_02 IN ACCESS SHARE MODE`, schema))
	require.NoError(t, err)

	start := time.Now()
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
		AcceptBlocking:      table,
	}, blockingDropOptions())
	elapsed := time.Since(start)
	require.NoError(t, reader.Rollback(t.Context()))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, res.Reason)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeRefused, res.Verdicts[0].Outcome)
	assert.Equal(t, verdict.CauseLockBudget, res.Verdicts[0].Cause)
	assert.Equal(t, verdict.CauseLockBudget, res.Cause)
	assert.Less(t, elapsed, 5*time.Second, "the lock budget, not the 10s statement budget, ended the wait")
	assert.Equal(t, 4, kindIndexCount(t, pool, schema), "nothing was dropped")
}

// An index re-created under the same name with another definition after
// the review is another plan: the pinned fingerprint no longer matches, so
// the run is refused before anything runs and the new index stands.
func TestRunDesiredBlockingIndexDropRefusesAnIndexChangedSinceReview(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.events_kind_idx;
		CREATE INDEX events_kind_idx ON %[1]s.events (kind, id);`, schema))
	require.NoError(t, err)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
		AcceptBlocking:      table,
	}, blockingDropOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
	assert.Empty(t, res.Verdicts)
	assert.Equal(t, 4, kindIndexCount(t, pool, schema), "nothing was dropped")
}

// The acceptance must name the desired file's table: another table is a
// usage error before anything runs.
func TestRunDesiredBlockingIndexDropRefusesAnAcceptanceForAnotherTable(t *testing.T) {
	pool, schema := seedEventsWithKindIndex(t)
	report := reviewKindIndexDrop(t, pool, schema)
	table := schema + ".events"

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, eventsWithoutKindIndex),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    table,
		AcceptBlocking:      schema + ".orders",
	}, blockingDropOptions())
	require.ErrorIs(t, err, migrate.ErrAcceptBlockingMismatch)
	var mismatch *migrate.AcknowledgementMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Equal(t, table, mismatch.Table)
	assert.Equal(t, migrate.DesiredResult{}, res)
	assert.Equal(t, 4, kindIndexCount(t, pool, schema), "nothing was dropped")
}
