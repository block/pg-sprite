package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A range-partitioned parent renders as its own definition: the columns,
// the key constraints (each including the partition key, as PostgreSQL
// requires on a parent), the PARTITION BY clause, and every index on the
// whole table without the ONLY the server prints for a parent's index. The
// partitions are not rendered. The rendered file materializes on the
// scratch schema as a parent with no partitions, which introspects to the
// same model, so diffing it against the live parent plans nothing.
func TestRenderRoundTripsRangePartitionedParent(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint GENERATED ALWAYS AS IDENTITY,
			community_id bigint NOT NULL,
			created_at timestamptz NOT NULL,
			external_id uuid NOT NULL,
			kind text NOT NULL,
			kind_length int GENERATED ALWAYS AS (length(kind)) STORED,
			payload jsonb NOT NULL DEFAULT '{}',
			deleted_at timestamptz,
			CONSTRAINT events_pkey PRIMARY KEY (community_id, created_at, id),
			CONSTRAINT events_external_key UNIQUE (external_id, created_at),
			CONSTRAINT events_kind_check CHECK (kind <> '')
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;

		CREATE INDEX events_live_idx ON %[1]s.events (community_id, created_at DESC)
			WHERE deleted_at IS NULL;
		CREATE INDEX events_kind_lower_idx ON %[1]s.events (lower(kind));
		CREATE INDEX events_payload_idx ON %[1]s.events USING gin (payload);`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	assert.Equal(t, `CREATE TABLE "events" (
    "id" bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    "community_id" bigint NOT NULL,
    "created_at" timestamp with time zone NOT NULL,
    "external_id" uuid NOT NULL,
    "kind" text NOT NULL,
    "kind_length" integer GENERATED ALWAYS AS (length(kind)) STORED,
    "payload" jsonb DEFAULT '{}'::jsonb NOT NULL,
    "deleted_at" timestamp with time zone,
    CONSTRAINT "events_external_key" UNIQUE (external_id, created_at),
    CONSTRAINT "events_kind_check" CHECK ((kind <> ''::text)),
    CONSTRAINT "events_pkey" PRIMARY KEY (community_id, created_at, id)
) PARTITION BY RANGE (created_at);

CREATE INDEX events_kind_lower_idx ON events USING btree (lower(kind));

CREATE INDEX events_live_idx ON events USING btree (community_id, created_at DESC) WHERE deleted_at IS NULL;

CREATE INDEX events_payload_idx ON events USING gin (payload);
`, rendered)

	live, desired, changes := roundTrip(t, pool, schema, "events")
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a partitioned parent against its own rendering must yield no changes")
}

// A list-partitioned parent round-trips the same way: the key method is
// part of the PARTITION BY clause the server prints, and the rendered file
// reproduces it.
func TestRenderRoundTripsListPartitionedParent(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id bigint NOT NULL,
			region text NOT NULL,
			placed_at timestamptz NOT NULL,
			CONSTRAINT orders_pkey PRIMARY KEY (region, id)
		) PARTITION BY LIST (region);

		CREATE TABLE %[1]s.orders_eu PARTITION OF %[1]s.orders FOR VALUES IN ('eu');
		CREATE TABLE %[1]s.orders_us PARTITION OF %[1]s.orders FOR VALUES IN ('us');

		CREATE INDEX orders_placed_at_idx ON %[1]s.orders (placed_at);`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "orders")
	require.NoError(t, err)
	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	assert.Equal(t, `CREATE TABLE "orders" (
    "id" bigint NOT NULL,
    "region" text NOT NULL,
    "placed_at" timestamp with time zone NOT NULL,
    CONSTRAINT "orders_pkey" PRIMARY KEY (region, id)
) PARTITION BY LIST (region);

CREATE INDEX orders_placed_at_idx ON orders USING btree (placed_at);
`, rendered)

	live, desired, changes := roundTrip(t, pool, schema, "orders")
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a partitioned parent against its own rendering must yield no changes")
}

// A partitioned parent whose index is not attached on every partition —
// built ON ONLY the parent, with a partition that has no matching index —
// is invalid. The rendered file would declare the index as complete and the
// diff, which compares a parent's indexes by name and definition alone,
// would report no changes, so the render refuses with a typed error.
func TestRenderRefusesLivePartitionedParentWithUnattachedIndex(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.metrics (
			id bigint NOT NULL,
			day date NOT NULL
		) PARTITION BY RANGE (day);

		CREATE TABLE %[1]s.metrics_p1 PARTITION OF %[1]s.metrics
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');

		CREATE INDEX metrics_day_idx ON ONLY %[1]s.metrics (day);`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "metrics")
	require.NoError(t, err)
	require.Len(t, live.Indexes, 1)
	require.True(t, live.Indexes[0].Invalid, "an ON ONLY index with an unindexed partition is invalid")
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableUnattachedIndex)
}
