package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
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

// The events table's February partition holds the same (id, created_at) row
// twice, so the unique index cannot be built there. The build gives January
// its index and stops at February: the failed verdict is coded data-fault,
// because the rows are what the operator corrects, the process exits 1 and
// not with the refusal code, and the detail names the partition, its
// invalid index, and that a rerun meets the same rows until they are
// corrected. A rerun with the duplicate still there stops the same way. Once
// the duplicate is deleted, a rerun drops February's invalid index, builds
// it again, builds March's, and leaves the parent's index valid.
func TestMigrateReportsDuplicateRowsInAPartitionAsADataFault(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{`
		CREATE TABLE %s.events (
		    id         bigint NOT NULL,
		    created_at date NOT NULL,
		    payload    text
		) PARTITION BY RANGE (created_at)`, `
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`, `
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')`, `
		CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')`, `
		INSERT INTO %s.events (id, created_at, payload) VALUES
		    (1, '2026-01-05', 'january'),
		    (42, '2026-02-10', 'first copy'),
		    (42, '2026-02-10', 'second copy'),
		    (7, '2026-03-02', 'march')`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err)
	}
	alter := fmt.Sprintf(`CREATE UNIQUE INDEX events_id_uq ON %s.events (id, created_at)`, schema)
	run := func() (verdict.Verdict, error) {
		cmd := newMigrateCmd(url, alter)
		cmd.JSON = true
		var out strings.Builder
		runErr := cmd.run(t.Context(), &out)
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v), "the verdict is on stdout: %q", out.String())
		return v, runErr
	}

	first, err := run()
	require.Error(t, err, "a data fault is an operational error")
	require.NotErrorIs(t, err, verdict.ErrRefused, "a data fault is not a refusal: the process exits 1, not 2")
	assert.Equal(t, verdict.OutcomeFailed, first.Outcome)
	assert.Equal(t, string(executor.CodeDataFault), first.Code)
	assert.Equal(t, "23505", first.SQLState, "the server rejected the duplicate key")
	assert.Equal(t, schema+".events", first.Table)

	// The partition's index takes a name derived from the partition's OID,
	// so it is read from the catalog: the one invalid index on February.
	var partitionIndex string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT c.relname
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = ($1 || '.events_2026_02')::regclass
		   AND NOT i.indisvalid`, schema).Scan(&partitionIndex))
	partition := pgx.Identifier{schema, "events_2026_02"}.Sanitize()
	assert.Contains(t, first.Detail, "the rows of partition "+partition+" violate index "+pgx.Identifier{schema, partitionIndex}.Sanitize())
	assert.Contains(t, first.Detail, "every rerun meets the same rows until they are corrected")
	assert.Contains(t, first.Detail, "correct the rows, then run the same statement again", "a rerun is advised only once the rows are corrected")
	assert.NotContains(t, first.Detail, "fix the cause the error names", "the detail names the cause rather than pointing at the error")
	assert.Equal(t, 3, indexCountIn(t, pool, schema), "the parent's invalid index, January's, and February's invalid one")

	again, err := run()
	require.Error(t, err)
	assert.Equal(t, first, again, "a rerun over the same rows stops the same way")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`DELETE FROM %s.events WHERE payload = 'second copy'`, schema))
	require.NoError(t, err)
	fixed, err := run()
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, fixed.Outcome, "detail: %s", fixed.Detail)
	var invalid int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_index i
		  JOIN pg_class t ON t.oid = i.indrelid
		 WHERE t.relnamespace = $1::regnamespace
		   AND NOT i.indisvalid`, schema).Scan(&invalid))
	assert.Zero(t, invalid, "the rerun replaced February's invalid index and the parent's index is valid")
	assert.Equal(t, 4, indexCountIn(t, pool, schema), "the parent's index and one per partition")
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
