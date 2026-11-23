package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file for a partitioned parent — the form pull writes, plus one
// nullable column — converges the live parent in place: the diff plans the
// ADD COLUMN on the parent, which PostgreSQL carries to every partition, and
// the partitions themselves stay outside the file.
func TestRunDesiredAddsColumnToPartitionedParent(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created_at timestamptz NOT NULL,
			CONSTRAINT events_pkey PRIMARY KEY (created_at, id)
		) PARTITION BY RANGE (created_at);

		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');

		CREATE INDEX events_id_idx ON %[1]s.events (id);`, schema))
	require.NoError(t, err)

	desired := parseDesired(t, `CREATE TABLE "events" (
    "id" bigint NOT NULL,
    "created_at" timestamp with time zone NOT NULL,
    "note" text,
    CONSTRAINT "events_pkey" PRIMARY KEY (created_at, id)
) PARTITION BY RANGE (created_at);

CREATE INDEX events_id_idx ON events USING btree (id);`)
	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: desired}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)

	var partitionHasNote bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'events_2026_01' AND column_name = 'note')`, schema).Scan(&partitionHasNote))
	assert.True(t, partitionHasNote, "the column added on the parent reaches its partition")
}
