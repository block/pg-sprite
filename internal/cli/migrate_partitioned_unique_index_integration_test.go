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
	"github.com/block/pg-sprite/pkg/verdict"
)

// indexCountIn is how many indexes the tables of schema carry, partitioned
// or not, valid or not.
func indexCountIn(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_index i
		  JOIN pg_class t ON t.oid = i.indrelid
		 WHERE t.relnamespace = $1::regnamespace`, schema).Scan(&n))
	return n
}

// assertUniqueIndexRefusedLikeApply runs alter as a dry run and as an apply,
// and asserts both end in the same by-design refusal, which names the missing
// partition key column, and that neither created an index.
func assertUniqueIndexRefusedLikeApply(t *testing.T, url string, pool *pgxpool.Pool, schema, alter, missing string) {
	t.Helper()
	dryRun, applied := admissionRefusalVerdicts(t, url, alter)
	assert.Equal(t, verdict.OutcomeRefused, dryRun.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, dryRun.Reason)
	assert.Equal(t, verdict.ClassByDesign, dryRun.Class)
	assert.Equal(t, schema+".events", dryRun.Table)
	assert.Contains(t, dryRun.Detail, missing, "the refusal names the partition key column the index lacks")
	assert.Equal(t, applied, dryRun, "the dry run must report the refusal the apply reports")
	assert.Zero(t, indexCountIn(t, pool, schema), "neither run created an index")
}

// PostgreSQL requires a unique index on a partitioned table to include every
// partition key column. The events table is partitioned by created_at, which
// the index on (community_id, kind) lacks, so the server would reject the
// parent's index. pg-sprite refuses the statement before it creates
// anything, by design: the statement has to change, not the engine. The dry
// run reads the same partition keys and ends in the same refusal and exit
// code. With created_at in the key the index builds.
func TestMigrateRefusesAUniqueIndexMissingThePartitionKey(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{`
		CREATE TABLE %s.events (
		    id           bigint NOT NULL,
		    community_id bigint NOT NULL,
		    kind         text NOT NULL,
		    created_at   date NOT NULL
		) PARTITION BY RANGE (created_at)`, `
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err)
	}

	assertUniqueIndexRefusedLikeApply(t, url, pool, schema,
		fmt.Sprintf(`CREATE UNIQUE INDEX events_community_kind_key ON %s.events (community_id, kind)`, schema),
		"created_at")

	covering := newMigrateCmd(url, fmt.Sprintf(`CREATE UNIQUE INDEX events_id_key ON %s.events (id, created_at)`, schema))
	var out strings.Builder
	require.NoError(t, covering.run(t.Context(), &out))
	assert.Equal(t, 2, indexCountIn(t, pool, schema), "the parent's index and its one partition's")
}

// The events table is partitioned by region, which the index on (region, id)
// includes, but its eu partition is partitioned again by created_at, which
// it does not. The server would accept the parent's index and reject the eu
// partition's, leaving an invalid parent index behind; pg-sprite checks
// every partitioned table in the tree first and refuses with nothing
// created, in the dry run and the apply alike.
func TestMigrateRefusesAUniqueIndexMissingASubPartitionKey(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{`
		CREATE TABLE %s.events (
		    id         bigint NOT NULL,
		    region     text NOT NULL,
		    created_at date NOT NULL
		) PARTITION BY LIST (region)`, `
		CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events
		    FOR VALUES IN ('eu')
		    PARTITION BY RANGE (created_at)`, `
		CREATE TABLE %[1]s.events_eu_2026 PARTITION OF %[1]s.events_eu
		    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err)
	}

	assertUniqueIndexRefusedLikeApply(t, url, pool, schema,
		fmt.Sprintf(`CREATE UNIQUE INDEX events_region_id_key ON %s.events (region, id)`, schema),
		"created_at of "+schema+".events_eu")
}
