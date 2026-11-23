package executor_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// eventsByDay creates a table partitioned by day with two leaf partitions,
// and returns the partition names.
func (f partitionedFixture) eventsByDay(t *testing.T) []string {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.events (
		    id           bigint NOT NULL,
		    community_id bigint NOT NULL,
		    kind         text NOT NULL,
		    created_at   date NOT NULL
		) PARTITION BY RANGE (created_at)`, f.schema)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`, f.schema)
	f.exec(t, `
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')`, f.schema)
	return []string{"events_2026_01", "events_2026_02"}
}

// requireMissingPartitionKey asserts err is the build's refusal of a unique
// index that does not cover the tree's partition keys, naming exactly want.
func requireMissingPartitionKey(t *testing.T, err error, index string, want []executor.MissingPartitionKey) {
	t.Helper()
	require.ErrorIs(t, err, executor.ErrUniqueIndexMissingPartitionKey)
	keyErr, ok := errors.AsType[*executor.UniqueIndexPartitionKeyError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, index, keyErr.Index)
	assert.Equal(t, want, keyErr.Missing)
	assert.Equal(t, executor.CodeUniqueIndexMissingPartitionKey, executor.OutcomeCode(err))
}

// PostgreSQL requires a unique index on a partitioned table to include every
// partition key column of every partitioned table it is created on, and
// refuses a partition key that is an expression. The build checks the whole
// tree before it creates the parent's index, so such an index is refused with
// nothing created, at the parent or further down the tree, and the dry run's
// admission reports the same refusal.
func TestBuildPartitionedIndexRefusesAUniqueIndexMissingAPartitionKey(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		// The parent is partitioned by created_at, which the index's key
		// lacks: the server would refuse the parent's ON ONLY index.
		t.Run("a key without the parent's partition column", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.eventsByDay(t)
			sql := fmt.Sprintf(`CREATE UNIQUE INDEX events_community_kind_key ON %s.events (community_id, kind)`, f.schema)
			want := []executor.MissingPartitionKey{{Table: f.schema + ".events", Columns: []string{"created_at"}}}

			_, err := f.buildErr(t, "events", sql)

			requireMissingPartitionKey(t, err, f.schema+".events_community_kind_key", want)
			f.requireNothingBuilt(t, "events", partitions...)
			admitErr := executor.AdmitPartitionedUniqueIndex(t.Context(), f.pool, sql)
			assert.Equal(t, err, admitErr, "the dry run's admission refuses with the build's own error")
		})

		// The parent is partitioned by region, which the key has, but each
		// region is partitioned again by created_at, which it lacks. The
		// server accepts the parent's ON ONLY index and refuses the first
		// region's, so a build that did not check the whole tree first would
		// leave an invalid parent index behind.
		t.Run("a key without a sub-partition's partition column", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %s.events (
				    id         bigint NOT NULL,
				    region     text NOT NULL,
				    created_at date NOT NULL
				) PARTITION BY LIST (region)`, f.schema)
			f.exec(t, `
				CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events
				    FOR VALUES IN ('eu')
				    PARTITION BY RANGE (created_at)`, f.schema)
			f.exec(t, `
				CREATE TABLE %[1]s.events_eu_2026 PARTITION OF %[1]s.events_eu
				    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`, f.schema)
			f.exec(t, `
				CREATE TABLE %[1]s.events_us PARTITION OF %[1]s.events
				    FOR VALUES IN ('us')`, f.schema)
			sql := fmt.Sprintf(`CREATE UNIQUE INDEX events_region_id_key ON %s.events (region, id)`, f.schema)
			want := []executor.MissingPartitionKey{{Table: f.schema + ".events_eu", Columns: []string{"created_at"}}}

			_, err := f.buildErr(t, "events", sql)

			requireMissingPartitionKey(t, err, f.schema+".events_region_id_key", want)
			f.requireNothingBuilt(t, "events", "events_eu", "events_eu_2026", "events_us")
			admitErr := executor.AdmitPartitionedUniqueIndex(t.Context(), f.pool, sql)
			assert.Equal(t, err, admitErr, "the dry run's admission refuses with the build's own error")
		})

		// A partition key that is an expression cannot be covered by any
		// column, even one the expression reads.
		t.Run("a parent partitioned by an expression", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `
				CREATE TABLE %s.tags (
				    id   bigint NOT NULL,
				    name text NOT NULL
				) PARTITION BY LIST (lower(name))`, f.schema)
			f.exec(t, `
				CREATE TABLE %[1]s.tags_a PARTITION OF %[1]s.tags
				    FOR VALUES IN ('a')`, f.schema)
			sql := fmt.Sprintf(`CREATE UNIQUE INDEX tags_name_key ON %s.tags (name, id)`, f.schema)

			_, err := f.buildErr(t, "tags", sql)

			requireMissingPartitionKey(t, err, f.schema+".tags_name_key",
				[]executor.MissingPartitionKey{{Table: f.schema + ".tags", Expression: true}})
			f.requireNothingBuilt(t, "tags", "tags_a")
		})

		// The same tree as the first case with created_at in the key: the
		// index covers the partition key, and the build runs to the end. A
		// non-unique index needs no partition key column at all.
		t.Run("a key with every partition column, and a non-unique index", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.eventsByDay(t)
			unique := fmt.Sprintf(`CREATE UNIQUE INDEX events_id_key ON %s.events (id, created_at)`, f.schema)
			plain := fmt.Sprintf(`CREATE INDEX events_community_kind_idx ON %s.events (community_id, kind)`, f.schema)

			require.NoError(t, executor.AdmitPartitionedUniqueIndex(t.Context(), f.pool, unique))
			require.NoError(t, executor.AdmitPartitionedUniqueIndex(t.Context(), f.pool, plain))
			f.build(t, "events", unique)
			f.build(t, "events", plain)

			f.requireCovered(t, "events", "events_id_key", partitions...)
			f.requireCovered(t, "events", "events_community_kind_idx", partitions...)
		})
	})
}
