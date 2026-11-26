package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedEventsFixture is a range-partitioned table with two leaf
// partitions and one partition list-partitioned on region, a CHECK on the
// parent that every partition inherits, and a row in every leaf.
const partitionedEventsFixture = `
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
INSERT INTO %[1]s.events VALUES
	(1, '2024-05-01', 'eu', 'a', 'x'),
	(2, '2025-05-01', 'us', 'b', 'y'),
	(3, '2026-05-01', 'eu', 'c', 'z'),
	(4, '2026-06-01', 'us', 'd', 'w');`

// partitionedEventsTree is the parent and every partition of the fixture.
var partitionedEventsTree = []string{"events", "events_2024", "events_2025", "events_2026", "events_2026_eu", "events_2026_us"}

func partitionedEventsTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(partitionedEventsFixture, schema))
	require.NoError(t, err)
	return pool, schema
}

// treeTablesWith names the tables of the partition tree that have a column,
// or a constraint, of the given name.
func treeTablesWith(t *testing.T, pool *pgxpool.Pool, schema, query, name string) []string {
	t.Helper()
	var out []string
	for _, table := range partitionedEventsTree {
		var n int
		require.NoError(t, pool.QueryRow(t.Context(), query, schema+"."+table, name).Scan(&n))
		if n > 0 {
			out = append(out, table)
		}
	}
	return out
}

const (
	columnCount     = `SELECT count(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2 AND NOT attisdropped`
	constraintCount = `SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`
)

// runReviewedEvents derives the plan for desired, pins it, and runs it with
// the destructive acknowledgement for the events table, the way an operator
// runs a plan they reviewed.
func runReviewedEvents(t *testing.T, pool *pgxpool.Pool, schema, desired string) migrate.DesiredResult {
	t.Helper()
	reviewed := plannedDrop(t, pool, schema, desired)
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, desired),
		ExpectedFingerprint: reviewed.Fingerprint,
		AllowDestructive:    schema + ".events",
	}, runOptions())
	if res.Outcome == verdict.OutcomeFailed {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	return res
}

// A desired file without the legacy column plans one native column drop on
// the partitioned parent. Pinned and acknowledged, it runs like the same
// drop on an ordinary table: the column leaves the parent and every
// partition, sub-partitions included, in one metadata-only change, the
// verdict records the acknowledgement, and the table converges on the
// file. A partition's column is the parent's, so the reviewed parent column
// is the whole of what the drop removes.
func TestRunDesiredDropsAColumnFromEveryPartition(t *testing.T) {
	pool, schema := partitionedEventsTable(t)
	desired := `CREATE TABLE events (
		id bigint NOT NULL,
		created_at timestamptz NOT NULL,
		region text NOT NULL,
		note text,
		CONSTRAINT events_note_short CHECK (length(note) < 100)
	) PARTITION BY RANGE (created_at);`

	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.events DROP legacy", schema), reviewed.Statements[0].SQL)
	assert.True(t, reviewed.Statements[0].Destructive)
	assert.Equal(t, planner.RouteNative, reviewed.Statements[0].Route)

	res := runReviewedEvents(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)
	assert.True(t, res.Verdicts[0].AllowedDestructive)
	assert.Empty(t, treeTablesWith(t, pool, schema, columnCount, "legacy"), "no table of the tree keeps the column")
	assert.Equal(t, partitionedEventsTree, treeTablesWith(t, pool, schema, columnCount, "note"))
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.events`, schema)).Scan(&rows))
	assert.Equal(t, 4, rows)
}

// A desired file without the parent's CHECK drops it from the parent and
// from every partition, which hold it only as an inherited copy.
func TestRunDesiredDropsAConstraintFromEveryPartition(t *testing.T) {
	pool, schema := partitionedEventsTable(t)
	desired := `CREATE TABLE events (
		id bigint NOT NULL,
		created_at timestamptz NOT NULL,
		region text NOT NULL,
		note text,
		legacy text
	) PARTITION BY RANGE (created_at);`
	require.Equal(t, partitionedEventsTree, treeTablesWith(t, pool, schema, constraintCount, "events_note_short"))

	res := runReviewedEvents(t, pool, schema, desired)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	assert.Empty(t, treeTablesWith(t, pool, schema, constraintCount, "events_note_short"))
}

// A desired file without region, the key the 2026 partition is
// list-partitioned on, plans a column drop PostgreSQL would refuse. The
// planner routes it to refuse with the partition-key reason, so admission
// refuses the plan whole and nothing runs.
func TestRunDesiredRefusesDroppingASubPartitionKeyColumn(t *testing.T) {
	pool, schema := partitionedEventsTable(t)
	desired := `CREATE TABLE events (
		id bigint NOT NULL,
		created_at timestamptz NOT NULL,
		note text,
		legacy text,
		CONSTRAINT events_note_short CHECK (length(note) < 100)
	) PARTITION BY RANGE (created_at);`
	reviewed := plannedDrop(t, pool, schema, desired)
	require.Len(t, reviewed.Statements, 1)
	require.Len(t, reviewed.Statements[0].Decisions, 1)
	assert.Equal(t, planner.ReasonPartitionKey, reviewed.Statements[0].Decisions[0].Reason)

	res := runReviewedEvents(t, pool, schema, desired)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
	assert.Empty(t, res.Verdicts, "nothing ran")
	assert.Equal(t, partitionedEventsTree, treeTablesWith(t, pool, schema, columnCount, "region"))
}

// An index a partition carries of its own on the column goes with the
// column when the parent drops it, and the plan reviewed only the parent's
// column. The drop fails with the executor's typed code, naming the index,
// and nothing is dropped.
func TestRunDesiredRefusesAColumnAPartitionIndexesOnItsOwn(t *testing.T) {
	pool, schema := partitionedEventsTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE INDEX events_2025_legacy_idx ON %s.events_2025 (legacy)`, schema))
	require.NoError(t, err)
	desired := `CREATE TABLE events (
		id bigint NOT NULL,
		created_at timestamptz NOT NULL,
		region text NOT NULL,
		note text,
		CONSTRAINT events_note_short CHECK (length(note) < 100)
	) PARTITION BY RANGE (created_at);`

	res := runReviewedEvents(t, pool, schema, desired)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, string(executor.CodeDroppedColumnPartitionDependent), res.Verdicts[0].Code)
	assert.Equal(t, partitionedEventsTree, treeTablesWith(t, pool, schema, columnCount, "legacy"), "every table keeps the column")
	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'events_2025_legacy_idx'`, schema).Scan(&n))
	assert.Equal(t, 1, n, "the partition's index survives")
}
