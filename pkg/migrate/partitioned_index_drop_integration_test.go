package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// seedPartitionedEvents creates a range-partitioned events table with one
// partition and an index on the parent. PostgreSQL makes the parent's index
// a partitioned index and builds ev_1_name_idx on the partition, attached
// under it.
func seedPartitionedEvents(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ev (
			id bigint NOT NULL,
			name text,
			day date NOT NULL
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.ev_1 PARTITION OF %[1]s.ev
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE INDEX ev_name_idx ON %[1]s.ev (name)`, schema))
	require.NoError(t, err)
	return schema
}

func partitionedDropPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// assertPartitionedIndexDropRefused checks the refusal a drop of a
// partitioned index ends in: the by-design partitioned-parent cause the
// declarative front door uses for the same drop, with no safer idiom, since
// PostgreSQL rejects the concurrent drop for this index.
func assertPartitionedIndexDropRefused(t *testing.T, v verdict.Verdict, schema string) {
	t.Helper()
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Equal(t, verdict.CauseParentConcurrentIndexDrop, v.Cause)
	assert.Empty(t, v.SaferIdiom, "PostgreSQL rejects DROP INDEX CONCURRENTLY for a partitioned index")
	assert.Equal(t, schema+".ev", v.Table)
	assert.False(t, v.BlockingPassthrough)
}

// A plain DROP INDEX of a partitioned table's index is refused, but not
// pointed at DROP INDEX CONCURRENTLY: PostgreSQL rejects that for a
// partitioned index in every supported version. The refusal names the
// bounded path instead — the same plain drop under --accept-blocking and
// --allow-destructive — and stays eligible for it.
func TestRunRefusesPlainDropOfPartitionedIndex(t *testing.T) {
	pool := partitionedDropPool(t)
	schema := seedPartitionedEvents(t, pool)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.ev_name_idx", schema)),
		runOptions())
	require.NoError(t, err)
	assertPartitionedIndexDropRefused(t, v, schema)
	assert.Contains(t, v.Detail, "--accept-blocking "+schema+".ev")
	assert.Contains(t, v.Detail, "--allow-destructive "+schema+".ev")

	proof, err := v.Refusal()
	require.NoError(t, err)
	assert.True(t, verdict.AcceptedBlockingEligible(proof),
		"the plain drop is the one PostgreSQL accepts, so the acknowledgement must reach it")
	assert.True(t, indexExists(t, pool, schema, "ev_name_idx"), "a refusal executes nothing")
}

// Under both acknowledgements the plain drop runs as submitted: the parent
// index and the partition's index attached under it are gone, and the
// verdict records the accepted refusal's identity and that every partition
// and partition index was locked, not the parent alone.
func TestRunAcceptBlockingDropsPartitionedIndex(t *testing.T) {
	pool := partitionedDropPool(t)
	schema := seedPartitionedEvents(t, pool)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.ev_name_idx", schema)),
		acceptBlockingOptions(schema+".ev"))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
	assert.True(t, v.BlockingPassthrough)
	assert.True(t, v.AllowedDestructive)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
	assert.Equal(t, verdict.CauseParentConcurrentIndexDrop, v.Cause)
	assert.Equal(t, schema+".ev", v.Table)
	assert.Contains(t, v.Detail, schema+".ev, every partition of it, and every partition's index were locked")
	assert.False(t, indexExists(t, pool, schema, "ev_name_idx"), "the accepted drop must have committed")
	assert.False(t, indexExists(t, pool, schema, "ev_1_name_idx"), "the partition's index goes with its parent")
}

// DROP INDEX CONCURRENTLY of a partitioned table's index is not "already
// safe, run it directly": PostgreSQL rejects it. It is refused with the
// partitioned-parent cause, and --accept-blocking cannot run it either —
// the passthrough runs the submitted statement inside a transaction, which a
// concurrent drop cannot run in — so the refusal stands under both
// acknowledgements and the index stays.
func TestRunRefusesConcurrentDropOfPartitionedIndex(t *testing.T) {
	pool := partitionedDropPool(t)
	schema := seedPartitionedEvents(t, pool)
	sql := fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_name_idx", schema)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
	require.NoError(t, err)
	assertPartitionedIndexDropRefused(t, v, schema)
	assert.Contains(t, v.Detail, "--accept-blocking "+schema+".ev")

	accepted, err := migrate.Run(t.Context(), pool, parseOne(t, sql), acceptBlockingOptions(schema+".ev"))
	require.NoError(t, err)
	assertPartitionedIndexDropRefused(t, accepted, schema)
	assert.True(t, indexExists(t, pool, schema, "ev_name_idx"), "a refusal executes nothing")
}

// assertAttachedIndexDropRefused checks the refusal a drop of an index
// attached under a parent index ends in: PostgreSQL drops such an index
// only together with its parent, in either form, so the refusal is by
// design, no acknowledgement reaches it, and the detail names the root
// partitioned index to drop instead.
func assertAttachedIndexDropRefused(t *testing.T, v verdict.Verdict, table, rootIndex string) {
	t.Helper()
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Empty(t, v.Cause)
	assert.Empty(t, v.SaferIdiom)
	assert.Equal(t, table, v.Table)
	assert.False(t, v.BlockingPassthrough)
	assert.Contains(t, v.Detail, "attached under the partitioned index "+rootIndex)
}

// A partition's index attached under the parent's index cannot be dropped
// on its own — PostgreSQL answers either form with "cannot drop index ...
// because index ... requires it" — so pg-sprite refuses it up front, plain
// or concurrent, with or without the acknowledgements, and names the parent
// index to drop instead.
func TestRunRefusesDropOfAttachedPartitionIndex(t *testing.T) {
	pool := partitionedDropPool(t)
	schema := seedPartitionedEvents(t, pool)

	for _, sql := range []string{
		fmt.Sprintf("DROP INDEX %s.ev_1_name_idx", schema),
		fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_1_name_idx", schema),
	} {
		t.Run(sql, func(t *testing.T) {
			v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), runOptions())
			require.NoError(t, err)
			assertAttachedIndexDropRefused(t, v, schema+".ev_1", schema+".ev_name_idx")

			accepted, err := migrate.Run(t.Context(), pool, parseOne(t, sql), acceptBlockingOptions(schema+".ev_1"))
			require.NoError(t, err)
			assertAttachedIndexDropRefused(t, accepted, schema+".ev_1", schema+".ev_name_idx")
			assert.True(t, indexExists(t, pool, schema, "ev_1_name_idx"), "a refusal executes nothing")
		})
	}

	// The contrast: an index created on the partition alone is attached
	// under nothing, so it is an ordinary index PostgreSQL drops
	// concurrently, and the plain drop keeps the gate's concurrent idiom.
	t.Run("an unattached index on the partition keeps the concurrent idiom", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE INDEX ev_1_day_idx ON %s.ev_1 (day)`, schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP INDEX %s.ev_1_day_idx", schema)),
			runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonIndexStatement, v.Reason)
		assert.Equal(t, "DROP INDEX CONCURRENTLY", v.SaferIdiom)
	})
}

// In a two-level partition tree the middle index is itself a partitioned
// index, and attached: PostgreSQL drops it only with the root, so the
// refusal names the root index, not its immediate parent.
func TestRunRefusesDropOfAttachedSubpartitionIndex(t *testing.T) {
	pool := partitionedDropPool(t)
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ev (
			id bigint NOT NULL,
			name text,
			region text NOT NULL,
			day date NOT NULL
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.ev_1 PARTITION OF %[1]s.ev
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')
			PARTITION BY LIST (region);
		CREATE TABLE %[1]s.ev_1_eu PARTITION OF %[1]s.ev_1
			FOR VALUES IN ('eu');
		CREATE INDEX ev_name_idx ON %[1]s.ev (name)`, schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("DROP INDEX %s.ev_1_name_idx", schema)),
		runOptions())
	require.NoError(t, err)
	assertAttachedIndexDropRefused(t, v, schema+".ev_1", schema+".ev_name_idx")
}
