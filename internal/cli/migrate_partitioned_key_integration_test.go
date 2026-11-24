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
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedKeyFixture creates a range-partitioned events table with two
// partitions and rows in each, and returns the schema. The partition key is
// created_at, so a key must include it; code allows NULL, so a primary key
// over it is refused.
func partitionedKeyFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
		    id         bigint NOT NULL,
		    created_at date NOT NULL,
		    code       text
		) PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		INSERT INTO %[1]s.events (id, created_at, code)
		SELECT g, DATE '2026-01-01' + (g %% 59), 'c' || g
		  FROM generate_series(1, 2000) g`, schema))
	require.NoError(t, err)
	return schema
}

// keyAdoption is one parent constraint as the catalog holds it: its kind,
// and for every partition the constraint it adopted.
type keyAdoption struct {
	contype string
	// adopted maps each partition to the name of its constraint attached to
	// the parent's.
	adopted map[string]string
}

// readKeyAdoption reads the parent constraint named name on schema.events and
// the partition constraints attached to it; found is false when the parent
// has no constraint under the name.
func readKeyAdoption(t *testing.T, pool *pgxpool.Pool, schema, name string) (keyAdoption, bool) {
	t.Helper()
	var k keyAdoption
	var parentOID uint32
	err := pool.QueryRow(t.Context(), `
		SELECT oid, contype::text FROM pg_constraint
		 WHERE conrelid = to_regclass($1) AND conname = $2`,
		schema+".events", name).Scan(&parentOID, &k.contype)
	if err != nil {
		require.ErrorContains(t, err, "no rows")
		return keyAdoption{}, false
	}
	rows, err := pool.Query(t.Context(), `
		SELECT c.conrelid::regclass::text, c.conname
		  FROM pg_constraint c
		 WHERE c.conparentid = $1`, parentOID)
	require.NoError(t, err)
	defer rows.Close()
	k.adopted = map[string]string{}
	for rows.Next() {
		var table, constraint string
		require.NoError(t, rows.Scan(&table, &constraint))
		k.adopted[table] = constraint
	}
	require.NoError(t, rows.Err())
	return k, true
}

// assertKeyOnEveryPartition asserts the parent carries constraint name of
// kind contype, valid, and that both partitions' constraints are attached to
// it.
func assertKeyOnEveryPartition(t *testing.T, pool *pgxpool.Pool, schema, name, contype string) {
	t.Helper()
	k, found := readKeyAdoption(t, pool, schema, name)
	require.True(t, found, "the parent carries constraint %s", name)
	assert.Equal(t, contype, k.contype)
	assert.Len(t, k.adopted, 2, "both partitions' constraints are attached to the parent's: %v", k.adopted)
	for _, partition := range []string{"events_2026_01", "events_2026_02"} {
		assert.Contains(t, k.adopted, schema+"."+partition)
	}
	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT i.indisvalid FROM pg_constraint c JOIN pg_index i ON i.indexrelid = c.conindid
		 WHERE c.conrelid = to_regclass($1) AND c.conname = $2`, schema+".events", name).Scan(&valid))
	assert.True(t, valid, "the parent's index is valid")
}

// assertKeyDryRunPlansTheBuild runs alter as a dry run and asserts the plan
// is the partitioned key build: one executable step that is the statement
// itself, with no plain table's USING INDEX sequence for a reader to mistake
// for what runs.
func assertKeyDryRunPlansTheBuild(t *testing.T, url, alter string) {
	t.Helper()
	report := dryRunPlan(t, url, alter)
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	assert.Equal(t, router.DispositionExecute, st.Disposition)
	assert.Equal(t, planner.ExecutionPartitionedKeyBuild, st.Execution)
	assert.Equal(t, []string{st.SQL}, st.ExecSQL)
	for _, decision := range st.Decisions {
		assert.Equal(t, st.ExecSQL, decision.SaferSQL)
		assert.Equal(t, planner.ExecutionPartitionedKeyBuild, decision.SaferSQLExecution)
	}
}

// runKey runs alter through migrate --alter --json and returns the verdict.
func runKey(t *testing.T, url, alter string) (verdict.Verdict, error) {
	t.Helper()
	cmd := newMigrateCmd(url, alter)
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	return decodeVerdict(t, out.String()), err
}

// A primary key on a partitioned table runs as the partitioned key build:
// each partition's unique index is built concurrently and given the
// constraint, then the parent's ADD CONSTRAINT adopts them. PostgreSQL's own
// plain ADD PRIMARY KEY would build both partitions' indexes under ACCESS
// EXCLUSIVE on the whole tree, and its USING INDEX form is refused on a
// partitioned table, so this is the only online way. The dry run plans the
// build and the apply ends executed, with the key on every partition.
// Running the statement again finds the finished key and changes nothing.
func TestMigrateAlterAddsAPrimaryKeyToAPartitionedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)
	alter := fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema)

	assertKeyDryRunPlansTheBuild(t, url, alter)
	_, found := readKeyAdoption(t, pool, schema, "events_pkey")
	assert.False(t, found, "the dry run adds nothing")

	v, err := runKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".events", v.Table)
	assert.Empty(t, v.ExecutedSQL, "the build is one engine step, not a sequence of statements")
	assert.Contains(t, v.Detail, "the parent's ADD CONSTRAINT events_pkey adopted them")
	assert.Contains(t, v.Detail, "2 partitions, 2 built, 0 constrained from an existing index, 0 found already constrained")
	assertKeyOnEveryPartition(t, pool, schema, "events_pkey", "p")

	again, err := runKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Contains(t, again.Detail, "already had constraint events_pkey, adopted on all 2 partitions; nothing was changed")
}

// A unique constraint over a nullable column runs the same way: only a
// primary key needs its columns NOT NULL.
func TestMigrateAlterAddsAUniqueConstraintToAPartitionedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)
	alter := fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_code_key UNIQUE (code, created_at)`, schema)

	assertKeyDryRunPlansTheBuild(t, url, alter)

	v, err := runKey(t, url, alter)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Contains(t, v.Detail, "2 partitions, 2 built")
	assertKeyOnEveryPartition(t, pool, schema, "events_code_key", "u")
}

// assertKeyRefusedLikeApply runs alter as a dry run and as an apply and
// asserts both end in the same refusal of class, with exit code 2, and that
// neither added the constraint or built an index.
func assertKeyRefusedLikeApply(t *testing.T, url string, pool *pgxpool.Pool, schema, alter string, class verdict.Class, detail string) {
	t.Helper()
	dryRun, applied := admissionRefusalVerdicts(t, url, alter)
	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, class, dryRun.Class)
	assert.Equal(t, schema+".events", dryRun.Table)
	assert.Contains(t, dryRun.Detail, detail)
	assert.Contains(t, dryRun.Detail, "nothing was executed")
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
	assert.Zero(t, indexCountIn(t, pool, schema), "neither run built an index")
}

// The build names each partition's index and constraint after the parent's
// constraint, and a rerun finds them by those names, so an unnamed key, whose
// name the server would choose, is refused before anything runs, in the dry
// run and the apply alike. The engine could learn to choose the name itself,
// so it is a capability boundary.
func TestMigrateRefusesAnUnnamedKeyOnAPartitionedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)

	assertKeyRefusedLikeApply(t, url, pool, schema,
		fmt.Sprintf(`ALTER TABLE %s.events ADD PRIMARY KEY (id, created_at)`, schema),
		verdict.ClassCapabilityBoundary, "the constraint has no name")
}

// PostgreSQL requires a key on a partitioned table to include every
// partition key column. The key on id alone lacks created_at, so the server
// would reject the parent's constraint after the partitions' indexes were
// built; pg-sprite reads the partition keys first and refuses with nothing
// built, by design, in the dry run and the apply alike.
func TestMigrateRefusesAKeyMissingThePartitionKey(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)

	assertKeyRefusedLikeApply(t, url, pool, schema,
		fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id)`, schema),
		verdict.ClassByDesign, "created_at")
}

// A primary key sets NOT NULL on its columns, which scans every partition
// under ACCESS EXCLUSIVE. code allows NULL, so the build refuses before it
// touches a partition. The table has to change (SET NOT NULL first), not
// the engine, so the refusal is environmental.
func TestMigrateRefusesAPrimaryKeyOverANullableColumnOfAPartitionedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)

	v, err := runKey(t, url, fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (code, created_at)`, schema))
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Contains(t, v.Detail, "code")
	assert.Zero(t, indexCountIn(t, pool, schema), "the refusal built no index")
}

// The build takes only leaf partitions directly under the parent. The events
// table's 2026 partition is partitioned again by month, so the key is refused
// before any partition is touched; supporting it is a capability the engine
// does not have yet.
func TestMigrateRefusesAKeyOnASubPartitionedTable(t *testing.T) {
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

	v, err := runKey(t, url, fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema))
	require.ErrorIs(t, err, verdict.ErrRefused)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Contains(t, v.Detail, schema+".events_2026")
	assert.Zero(t, indexCountIn(t, pool, schema), "the refusal built no index")
}

// --force runs the submitted form as written, which on a partitioned table
// would build every partition's index under ACCESS EXCLUSIVE on the whole
// tree. The force override bypasses the routing to the build, so the forced
// key meets the partitioned-parent refusal it always met, and builds nothing.
func TestMigrateAlterForcedPartitionedKeyStaysRefused(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := partitionedKeyFixture(t, pool)

	cmd := newMigrateCmd(url, fmt.Sprintf(`ALTER TABLE %s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`, schema))
	cmd.JSON = true
	cmd.Force = schema + ".events"
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	v := decodeVerdict(t, out.String())
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
	assert.Equal(t, verdict.CauseParentBlockingIndexBuild, v.Cause)
	assert.True(t, v.Forced)
	assert.Zero(t, indexCountIn(t, pool, schema), "a refusal builds no index")
}
