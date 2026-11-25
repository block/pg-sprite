package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// partitionedEvents is a range-partitioned table with two leaf partitions
// and one partition that is itself list-partitioned on region, a CHECK and
// an index on the parent that every partition holds a copy of, and a row
// in every leaf.
const partitionedEvents = `
CREATE TABLE %[1]s.events (
	id bigint NOT NULL,
	created_at timestamptz NOT NULL,
	region text NOT NULL,
	note text,
	legacy text,
	CONSTRAINT events_note_short CHECK (length(note) < 100)
) PARTITION BY RANGE (created_at);
CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
	FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events
	FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
	FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')
	PARTITION BY LIST (region);
CREATE TABLE %[1]s.events_2026_eu PARTITION OF %[1]s.events_2026 FOR VALUES IN ('eu');
CREATE TABLE %[1]s.events_2026_us PARTITION OF %[1]s.events_2026 FOR VALUES IN ('us');
CREATE INDEX events_legacy_idx ON %[1]s.events (legacy);
INSERT INTO %[1]s.events VALUES
	(1, '2024-05-01', 'eu', 'a', 'x'),
	(2, '2025-05-01', 'us', 'b', 'y'),
	(3, '2026-05-01', 'eu', 'c', 'z'),
	(4, '2026-06-01', 'us', 'd', 'w');`

// eventsTree is the parent and every partition of partitionedEvents.
var eventsTree = []string{"events", "events_2024", "events_2025", "events_2026", "events_2026_eu", "events_2026_us"}

func partitionedEventsPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(partitionedEvents, schema))
	require.NoError(t, err)
	return pool, schema
}

func runDroppingEvents(t *testing.T, pool *pgxpool.Pool, schema string, d schemadiff.DroppedObject, sql string) error {
	t.Helper()
	pt := mustPreflight(t, pool, schema, "events")
	_, err := executor.RunSequenceDropping(t.Context(), pool, pt, []string{sql}, runBudget, executor.DefaultRetryPolicy(), d)
	return err
}

func eventRows(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.events`, schema)).Scan(&n))
	return n
}

// A reviewed column drop on a partitioned table drops the column from the
// parent and from every partition, the sub-partitions included. A
// partition's column is the parent's: PostgreSQL will not drop it from the
// partition alone, so the parent's drop is the whole change. The parent's
// index on the column goes with it, and so do the partitions' indexes
// attached under it, which are copies of the index the plan lists.
func TestRunSequenceDroppingDropsAColumnFromEveryPartition(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "legacy")
	require.Len(t, d.Dependents, 1, "the plan lists the parent's index on the column")

	err := runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema))
	require.NoError(t, err)
	for _, table := range eventsTree {
		assert.False(t, columnExists(t, pool, schema, table, "legacy"), "%s keeps no copy of the column", table)
		assert.True(t, columnExists(t, pool, schema, table, "note"), "%s keeps its other columns", table)
	}
	assert.Equal(t, 4, eventRows(t, pool, schema), "a column drop removes no row")
}

// A reviewed CHECK drop on a partitioned table drops the constraint from the
// parent and from every partition, which hold it as an inherited copy that
// PostgreSQL will not drop on its own.
func TestRunSequenceDroppingDropsAConstraintFromEveryPartition(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedConstraint, "events_note_short")

	err := runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP CONSTRAINT events_note_short", schema))
	require.NoError(t, err)
	for _, table := range eventsTree {
		assert.False(t, constraintExists(t, pool, schema, table, "events_note_short"), "%s keeps no copy of the constraint", table)
	}
}

// A reviewed foreign key drop on a partitioned table drops the parent's
// foreign key and the clones PostgreSQL made of it on every partition,
// which carry the parent's as their parent constraint and cannot be
// dropped on their own.
func TestRunSequenceDroppingDropsAForeignKeyFromEveryPartition(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.regions (code text PRIMARY KEY);
		INSERT INTO %[1]s.regions VALUES ('eu'), ('us');
		ALTER TABLE %[1]s.events
			ADD CONSTRAINT events_region_fkey FOREIGN KEY (region) REFERENCES %[1]s.regions`, schema))
	require.NoError(t, err)
	for _, table := range eventsTree {
		require.True(t, constraintExists(t, pool, schema, table, "events_region_fkey"), "%s holds a clone", table)
	}
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedConstraint, "events_region_fkey")

	err = runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP CONSTRAINT events_region_fkey", schema))
	require.NoError(t, err)
	for _, table := range eventsTree {
		assert.False(t, constraintExists(t, pool, schema, table, "events_region_fkey"), "%s keeps no clone", table)
	}
}

// A column in a partition key, of the parent or of a sub-partitioned
// partition, is refused before the drop runs, read under the table's lock,
// and every table keeps the column. PostgreSQL refuses to drop a partition
// key column at any level, so the refusal names the table whose key holds
// it rather than ending in a server error.
func TestRunSequenceDroppingRefusesAPartitionKeyColumn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		column string
		keyOf  string
	}{
		{name: "the parent's key", column: "created_at", keyOf: "events"},
		{name: "a sub-partition's key", column: "region", keyOf: "events_2026"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := partitionedEventsPool(t)
			d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, tc.column)

			err := runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN %s", schema, tc.column))
			require.ErrorIs(t, err, executor.ErrDroppedColumnPartitionKey)
			assert.Equal(t, executor.CodeDroppedColumnPartitionKey, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), schema+"."+tc.keyOf)
			for _, table := range eventsTree {
				assert.True(t, columnExists(t, pool, schema, table, tc.column), "%s keeps the column", table)
			}
		})
	}
}

// A column a partition key reads inside an expression is a key column too.
func TestRunSequenceDroppingRefusesAColumnInAPartitionKeyExpression(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			region text NOT NULL
		) PARTITION BY LIST (lower(region));
		CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events FOR VALUES IN ('eu')`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "region")

	err = runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN region", schema))
	require.ErrorIs(t, err, executor.ErrDroppedColumnPartitionKey)
	assert.True(t, columnExists(t, pool, schema, "events_eu", "region"))
}

// An object a partition carries of its own on the dropped column, rather
// than as a copy of one of the parent's, goes with the column when the
// parent drops it. The plan reviewed the parent's column and its
// dependents only, so the drop is refused, read under the locks of the
// whole tree, the refusal names the object, and nothing is dropped.
func TestRunSequenceDroppingRefusesAColumnAPartitionUsesOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create string
		named  string
		alive  string
	}{
		{
			name:   "partition index",
			create: `CREATE INDEX events_2025_legacy_local ON %[1]s.events_2025 (legacy)`,
			named:  "index %[1]s.events_2025_legacy_local",
			alive:  `SELECT count(*) FROM pg_indexes WHERE schemaname = '%[1]s' AND indexname = 'events_2025_legacy_local'`,
		},
		{
			name:   "sub-partition index on several columns",
			create: `CREATE INDEX events_2026_eu_note_legacy ON %[1]s.events_2026_eu (note, legacy)`,
			named:  "index %[1]s.events_2026_eu_note_legacy",
			alive:  `SELECT count(*) FROM pg_indexes WHERE schemaname = '%[1]s' AND indexname = 'events_2026_eu_note_legacy'`,
		},
		{
			name: "partition check",
			create: `ALTER TABLE %[1]s.events_2024
				ADD CONSTRAINT events_2024_legacy_set CHECK (legacy IS NOT NULL)`,
			named: "table constraint events_2024_legacy_set on %[1]s.events_2024",
			alive: `SELECT count(*) FROM pg_constraint WHERE connamespace = '%[1]s'::regnamespace AND conname = 'events_2024_legacy_set'`,
		},
		{
			name: "partition foreign key",
			create: `CREATE TABLE %[1]s.legacy_codes (code text PRIMARY KEY);
				INSERT INTO %[1]s.legacy_codes VALUES ('x');
				ALTER TABLE %[1]s.events_2024
					ADD CONSTRAINT events_2024_legacy_fk FOREIGN KEY (legacy) REFERENCES %[1]s.legacy_codes`,
			named: "table constraint events_2024_legacy_fk on %[1]s.events_2024",
			alive: `SELECT count(*) FROM pg_constraint WHERE connamespace = '%[1]s'::regnamespace AND conname = 'events_2024_legacy_fk'`,
		},
		{
			name: "partition statistics",
			create: `CREATE STATISTICS %[1]s.events_2025_note_legacy (dependencies)
				ON note, legacy FROM %[1]s.events_2025`,
			named: "statistics object %[1]s.events_2025_note_legacy",
			alive: `SELECT count(*) FROM pg_statistic_ext WHERE stxnamespace = '%[1]s'::regnamespace AND stxname = 'events_2025_note_legacy'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := partitionedEventsPool(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.create, schema))
			require.NoError(t, err)
			d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "legacy")

			err = runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema))
			require.ErrorIs(t, err, executor.ErrDroppedColumnPartitionDependent)
			assert.Equal(t, executor.CodeDroppedColumnPartitionDependent, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), fmt.Sprintf(tc.named, schema))
			for _, table := range eventsTree {
				assert.True(t, columnExists(t, pool, schema, table, "legacy"), "%s keeps the column", table)
			}
			var n int
			require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(tc.alive, schema)).Scan(&n))
			assert.Equal(t, 1, n, "the partition's own object survives")
		})
	}
}

// A NOT NULL drop on a partitioned table stays refused: a partition can
// hold a NOT NULL of its own on the column, which the plan did not review.
// A column drop that says ONLY is refused too: PostgreSQL will not drop a
// column from a partitioned table alone, and a plan never says ONLY.
func TestRunSequenceDroppingRefusesANotNullOrOnlyDropOnAPartitionedTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind schemadiff.DroppedKind
		obj  string
		sql  string
	}{
		{name: "not null", kind: schemadiff.DroppedNotNull, obj: "region", sql: "ALTER TABLE %s.events ALTER COLUMN region DROP NOT NULL"},
		{name: "only", kind: schemadiff.DroppedColumn, obj: "legacy", sql: "ALTER TABLE ONLY %s.events DROP COLUMN legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := partitionedEventsPool(t)
			d := modeltest.PlannedDrop(t, pool, schema, "events", tc.kind, tc.obj)

			err := runDroppingEvents(t, pool, schema, d, fmt.Sprintf(tc.sql, schema))
			require.ErrorIs(t, err, executor.ErrDroppedObjectHasDescendants)
			assert.Equal(t, executor.CodeDroppedObjectHasDescendants, executor.OutcomeCode(err))
			assert.True(t, columnExists(t, pool, schema, "events_2024", "legacy"))
			assert.True(t, columnNotNull(t, pool, schema, "events_2024", "region"))
		})
	}
}

// Objects the parent holds on the dropped column are the parent's reviewed
// objects, and so are their copies on the partitions: a default, NOT NULL,
// an inherited CHECK, a UNIQUE constraint and a foreign key. With all of
// them on legacy,
//
//	ALTER TABLE events DROP COLUMN legacy
//
// runs, and no table of the tree keeps the column.
func TestRunSequenceDroppingDropsAColumnCarryingTheParentsOwnObjects(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.legacy_codes (
			code text PRIMARY KEY
		);
		INSERT INTO %[1]s.legacy_codes VALUES ('x'), ('y'), ('z'), ('w');
		ALTER TABLE %[1]s.events ALTER COLUMN legacy SET DEFAULT 'x';
		ALTER TABLE %[1]s.events ALTER COLUMN legacy SET NOT NULL;
		ALTER TABLE %[1]s.events ADD CONSTRAINT events_legacy_nonempty CHECK (legacy <> '');
		ALTER TABLE %[1]s.events ADD CONSTRAINT events_uq UNIQUE (created_at, region, legacy);
		ALTER TABLE %[1]s.events ADD CONSTRAINT events_legacy_fk FOREIGN KEY (legacy) REFERENCES %[1]s.legacy_codes;`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "legacy")

	require.NoError(t, runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema)))
	for _, table := range eventsTree {
		assert.False(t, columnExists(t, pool, schema, table, "legacy"), "%s keeps no copy of the column", table)
	}
}

// A partition created with its own NOT NULL and default on the column,
//
//	CREATE TABLE events_2027 PARTITION OF events (legacy WITH OPTIONS NOT NULL DEFAULT 'q') ...
//
// holds options on the parent's column, not objects of its own built on
// it, so the reviewed drop runs and events_2027 loses the column too. On
// PostgreSQL 18 the NOT NULL is a catalogued constraint, which the check
// leaves out like the parent's.
func TestRunSequenceDroppingDropsAColumnOfAPartitionWithItsOwnNotNullAndDefault(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events_2027 PARTITION OF %[1]s.events (
			legacy WITH OPTIONS NOT NULL DEFAULT 'q'
		) FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "legacy")

	require.NoError(t, runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema)))
	for _, table := range append(eventsTree, "events_2027") {
		assert.False(t, columnExists(t, pool, schema, table, "legacy"), "%s keeps no copy of the column", table)
	}
}

// A partitioned table with no partitions still has a partition key, so
//
//	ALTER TABLE events DROP COLUMN created_at
//
// is refused with the same typed outcome as on a table with partitions,
// not with PostgreSQL's own error, and the column stays.
func TestRunSequenceDroppingRefusesAKeyColumnOfAPartitionedTableWithNoPartitions(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created_at timestamptz NOT NULL,
			note text
		) PARTITION BY RANGE (created_at)`, schema))
	require.NoError(t, err)
	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "created_at")

	err = runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN created_at", schema))
	require.ErrorIs(t, err, executor.ErrDroppedColumnPartitionKey)
	assert.Equal(t, executor.CodeDroppedColumnPartitionKey, executor.OutcomeCode(err))
	assert.True(t, columnExists(t, pool, schema, "events", "created_at"))
}

// A partition whose DETACH PARTITION ... CONCURRENTLY was cancelled is left
// detach-pending, and PostgreSQL leaves it out of a parent's column drop,
// so it would keep its copy while the reviewed plan describes the whole
// tree. Here events_2025's detach is cancelled while another session reads
// the parent; the reviewed drop of legacy is then refused with
// partition-detach-pending, and every table keeps the column.
func TestRunSequenceDroppingRefusesAColumnDropOverADetachPendingPartition(t *testing.T) {
	pool, schema := partitionedEventsPool(t)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = holder.Exec(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.events", schema))
	require.NoError(t, err)
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), "SET statement_timeout = '700ms'")
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.events
			DETACH PARTITION %[1]s.events_2025 CONCURRENTLY`, schema))
	require.Error(t, err, "the detach waits on the reader and is cancelled")
	_, err = conn.Exec(t.Context(), "RESET statement_timeout")
	require.NoError(t, err)
	conn.Release()
	require.NoError(t, holder.Rollback(t.Context()))
	var pending bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT inhdetachpending
		FROM pg_catalog.pg_inherits
		WHERE inhrelid = $1::regclass`, schema+".events_2025").Scan(&pending))
	require.True(t, pending, "the cancelled detach left the partition detach-pending")

	d := modeltest.PlannedDrop(t, pool, schema, "events", schemadiff.DroppedColumn, "legacy")
	err = runDroppingEvents(t, pool, schema, d, fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema))
	require.ErrorIs(t, err, executor.ErrPartitionDetachPending)
	assert.Equal(t, executor.CodePartitionDetachPending, executor.OutcomeCode(err))
	for _, table := range eventsTree {
		assert.True(t, columnExists(t, pool, schema, table, "legacy"), "%s keeps the column: nothing was dropped", table)
	}
}
