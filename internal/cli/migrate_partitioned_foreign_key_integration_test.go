package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedForeignKeyFixture creates accounts and a range-partitioned
// events table with two partitions whose rows all reference an account, and
// returns the pool, the connection URL, the schema, and the server major.
func partitionedForeignKeyFixture(t *testing.T) (*pgxpool.Pool, string, string, int) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.accounts (
		    id bigint PRIMARY KEY
		);
		INSERT INTO %[1]s.accounts (id)
		SELECT g FROM generate_series(1, 100) g;
		CREATE TABLE %[1]s.events (
		    id         bigint NOT NULL,
		    created_at date NOT NULL,
		    account_id bigint
		) PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		INSERT INTO %[1]s.events (id, created_at, account_id)
		SELECT g, DATE '2026-01-01' + (g %% 59), 1 + (g %% 100)
		  FROM generate_series(1, 2000) g`, schema))
	require.NoError(t, err)
	return pool, url, schema, major
}

// requireBeforeNotValidOnPartitions skips a test of the partitioned foreign
// key build on PostgreSQL 18 and later, where the ordinary NOT VALID then
// VALIDATE route runs instead.
func requireBeforeNotValidOnPartitions(t *testing.T, major int) {
	t.Helper()
	if major >= 18 {
		t.Skipf("PostgreSQL %d accepts NOT VALID on a partitioned table, so the foreign key takes the ordinary route", major)
	}
}

// foreignKeyAdoption is the parent's foreign key as the catalog holds it.
type foreignKeyAdoption struct {
	validated bool
	// adopted maps each partition to whether its foreign key is attached
	// to the parent's (conparentid set to the parent's OID).
	adopted map[string]bool
}

// readForeignKeyAdoption reads the foreign key named name on schema.events and
// the partition foreign keys attached to it; found is false when the parent
// has no constraint under the name.
func readForeignKeyAdoption(t *testing.T, pool *pgxpool.Pool, schema, name string) (foreignKeyAdoption, bool) {
	t.Helper()
	var fk foreignKeyAdoption
	var parentOID uint32
	err := pool.QueryRow(t.Context(), `
		SELECT oid, convalidated
		  FROM pg_constraint
		 WHERE conrelid = to_regclass($1)
		   AND conname = $2
		   AND contype = 'f'`,
		schema+".events", name).Scan(&parentOID, &fk.validated)
	if err != nil {
		require.ErrorContains(t, err, "no rows")
		return foreignKeyAdoption{}, false
	}
	rows, err := pool.Query(t.Context(), `
		SELECT i.inhrelid::regclass::text,
		       EXISTS (
		           SELECT 1
		             FROM pg_constraint c
		            WHERE c.conrelid = i.inhrelid
		              AND c.conparentid = $2
		              AND c.convalidated)
		  FROM pg_inherits i
		 WHERE i.inhparent = to_regclass($1)`,
		schema+".events", parentOID)
	require.NoError(t, err)
	defer rows.Close()
	fk.adopted = map[string]bool{}
	for rows.Next() {
		var partition string
		var adopted bool
		require.NoError(t, rows.Scan(&partition, &adopted))
		fk.adopted[partition] = adopted
	}
	require.NoError(t, rows.Err())
	return fk, true
}

// partitionForeignKeysAttached counts the partition foreign keys attached to
// any parent's foreign key.
func partitionForeignKeysAttached(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_constraint c
		  JOIN pg_inherits i ON i.inhrelid = c.conrelid
		 WHERE i.inhparent = to_regclass($1)
		   AND c.contype = 'f'
		   AND c.conparentid <> 0`, schema+".events").Scan(&n))
	return n
}

// runForeignKey runs alter through migrate --alter --json and returns the
// verdict.
func runForeignKey(t *testing.T, url, alter string) (verdict.Verdict, error) {
	t.Helper()
	cmd := newMigrateCmd(url, alter)
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	return decodeVerdict(t, out.String()), err
}

// A foreign key on a partitioned table runs as the partitioned foreign key
// build on PostgreSQL 14 to 17: each partition is given the foreign key NOT
// VALID and validated while writes continue, then the parent's ADD
// CONSTRAINT adopts them. Those versions refuse NOT VALID on a partitioned
// table, and the plain statement validates every partition while it blocks
// writes to the whole tree, so this is the only online way. The dry run
// plans the build, and the apply ends executed with every partition's foreign
// key adopted by the parent's, which is validated. Running the statement
// again finds the finished foreign key and changes nothing.
func TestMigrateAlterAddsAForeignKeyToAPartitionedTable(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)
	alter := fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema)

	report := dryRunPlan(t, url, alter)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	planned := report.Statements[0]
	assert.Equal(t, planner.ExecutionPartitionedForeignKeyBuild, planned.Execution)
	assert.Equal(t, []string{planned.SQL}, planned.ExecSQL, "the one step is the statement; the build lists the partitions when it runs")
	_, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	assert.False(t, found, "the dry run adds nothing")

	v, err := runForeignKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".events", v.Table)
	assert.Empty(t, v.ExecutedSQL, "the build is one engine step, not a sequence of statements")
	assert.Contains(t, v.Detail, "the parent's ADD CONSTRAINT events_account_fk adopted them")
	assert.Contains(t, v.Detail, "2 partitions, 0 found already validated, 0 validated, 2 added")
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found, "the parent carries the foreign key")
	assert.True(t, fk.validated, "the parent's foreign key is validated")
	assert.Equal(t, map[string]bool{
		schema + ".events_2026_01": true,
		schema + ".events_2026_02": true,
	}, fk.adopted, "every partition's validated foreign key is attached to the parent's")

	again, err := runForeignKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Equal(t, "the table is partitioned and already had foreign key events_account_fk, adopted on all 2 partitions; nothing was changed", again.Detail)
}

// A row that references no account breaks the foreign key. The build adds
// the first partition's foreign key and validates it, adds the second's NOT
// VALID, and its validation fails on the orphan row, so the run ends in a
// failed verdict that carries the server's error naming that partition. The
// parent gets no foreign key, so nothing is adopted; the partitions' own
// foreign keys stay for a rerun to resume from once the row is fixed.
func TestMigrateAlterFailsAPartitionedForeignKeyOnAnOrphanRow(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.events (id, created_at, account_id)
		VALUES (9999, DATE '2026-02-14', 424242)`, schema))
	require.NoError(t, err)

	v, err := runForeignKey(t, url, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))
	require.Error(t, err)
	require.NotErrorIs(t, err, verdict.ErrRefused, "the build executed steps, so this is a failure, not a refusal")
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeDataFault), v.Code)
	assert.Equal(t, "23503", v.SQLState, "foreign_key_violation: the rows, not the engine, stopped the run")
	assert.Contains(t, v.ServerError, "events_2026_02", "the server's error names the partition holding the orphan row")
	assert.Contains(t, v.Detail, "run the same statement again")
	_, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	assert.False(t, found, "the parent's foreign key was never added")
	assert.Zero(t, partitionForeignKeysAttached(t, pool, schema), "no partition's foreign key was adopted")
}

// assertForeignKeyRefusedLikeApply runs alter as a dry run and as an apply
// and asserts both end in the same capability-boundary refusal, with exit
// code 2, whose detail contains detail, and that neither added a foreign key
// anywhere in the tree.
func assertForeignKeyRefusedLikeApply(t *testing.T, url string, pool *pgxpool.Pool, schema, alter, detail string) {
	t.Helper()
	dryRun, applied := admissionRefusalVerdicts(t, url, alter)
	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, dryRun.Class)
	assert.Equal(t, schema+".events", dryRun.Table)
	assert.Contains(t, dryRun.Detail, detail)
	assert.Contains(t, dryRun.Detail, "nothing was executed")
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
	var foreignKeys int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relnamespace = $1::regnamespace
		   AND c.contype = 'f'`, schema).Scan(&foreignKeys))
	assert.Zero(t, foreignKeys, "neither run added a foreign key")
}

// Each partition's foreign key takes a name derived from the parent's, and a
// rerun finds it by that name, so an unnamed foreign key, whose name the
// server would choose, is refused before anything runs, in the dry run and
// the apply alike. The engine could learn to choose the name itself, so it is
// a capability boundary.
func TestMigrateRefusesAnUnnamedForeignKeyOnAPartitionedTable(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)

	assertForeignKeyRefusedLikeApply(t, url, pool, schema, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema),
		"the foreign key has no name")
}

// ON DELETE SET NULL with a column list is a clause the parent's foreign key
// does not compare when it adopts a partition's, so the build cannot prove
// the partitions' foreign keys match; it refuses before anything runs, in
// the dry run and the apply alike.
func TestMigrateRefusesAPartitionedForeignKeyWithAnOnDeleteColumnList(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)

	assertForeignKeyRefusedLikeApply(t, url, pool, schema, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
		    ON DELETE SET NULL (account_id)`, schema),
		"an ON DELETE column list")
}

// The build takes only a plain table as the referenced table: a partitioned
// one makes the parent's statement clone the foreign key onto every
// referenced partition, which the build neither proves nor reads back. Only
// the catalog says ledgers is partitioned, so the dry run reads it as the
// build does and ends in the refusal the apply gives, before anything runs.
func TestMigrateRefusesAPartitionedForeignKeyToAPartitionedTableLikeApply(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ledgers (
		    id bigint PRIMARY KEY
		) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.ledgers_low PARTITION OF %[1]s.ledgers
		    FOR VALUES FROM (1) TO (1000)`, schema))
	require.NoError(t, err)

	assertForeignKeyRefusedLikeApply(t, url, pool, schema, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_ledger_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.ledgers (id)`, schema),
		schema+".ledgers is a partitioned table, not a plain table")
}

// --force runs the submitted form as written: the plain ADD FOREIGN KEY,
// which validates every partition under SHARE ROW EXCLUSIVE on the whole
// tree, bounded by the brief budgets. The override bypasses the routing to
// the build, so the forced run is the one it always was: a small table
// finishes within the budget and the verdict records the override.
func TestMigrateAlterForcedPartitionedForeignKeyRunsAsWritten(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	requireBeforeNotValidOnPartitions(t, major)

	cmd := newMigrateCmd(url, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))
	cmd.JSON = true
	cmd.Force = schema + ".events"
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	v := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.True(t, v.Forced)
	assert.NotContains(t, v.Detail, "partition by partition", "the forced run is not the build")
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found)
	assert.True(t, fk.validated)
}

// PostgreSQL 18 accepts NOT VALID on a partitioned table, so the foreign key
// takes the ordinary online route there: ADD ... NOT VALID, then VALIDATE
// CONSTRAINT, planned and run as two steps, not the partitioned build.
func TestMigrateAlterPartitionedForeignKeyTakesTheOrdinaryRouteOnPostgres18(t *testing.T) {
	pool, url, schema, major := partitionedForeignKeyFixture(t)
	if major < 18 {
		t.Skipf("PostgreSQL %d refuses NOT VALID on a partitioned table, so the partitioned build runs instead", major)
	}
	alter := fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema)

	report := dryRunPlan(t, url, alter)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, planner.ExecutionAutocommit, report.Statements[0].Execution)
	require.Len(t, report.Statements[0].ExecSQL, 2, "ADD ... NOT VALID, then VALIDATE CONSTRAINT")

	v, err := runForeignKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Len(t, v.ExecutedSQL, 2)
	fk, found := readForeignKeyAdoption(t, pool, schema, "events_account_fk")
	require.True(t, found)
	assert.True(t, fk.validated)
}
