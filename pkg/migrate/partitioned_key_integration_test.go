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
	"github.com/block/pg-sprite/pkg/verdict"
)

// partitionedOrders creates billing-style orders partitioned by placed_at,
// with two partitions holding rows, and returns the pool and schema.
func partitionedOrders(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
		    id        bigint NOT NULL,
		    placed_at date NOT NULL
		) PARTITION BY RANGE (placed_at);
		CREATE TABLE %[1]s.orders_2026_01 PARTITION OF %[1]s.orders
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.orders_2026_02 PARTITION OF %[1]s.orders
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		INSERT INTO %[1]s.orders (id, placed_at)
		SELECT g, DATE '2026-01-01' + (g %% 59) FROM generate_series(1, 500) g`, schema))
	require.NoError(t, err)
	return pool, url, schema
}

// keyOptions is runOptions wired with the connection the key build takes its
// table lock from.
func keyOptions(url string) migrate.Options {
	opts := runOptions()
	opts.Connection = dbconn.Config{URL: url}
	return opts
}

func constraintsPrefixed(t *testing.T, pool *pgxpool.Pool, schema, name string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relnamespace = $1::regnamespace AND c.conname LIKE $2 || '%'`, schema, name).Scan(&n))
	return n
}

// migrate.Run sends an ADD PRIMARY KEY on a partitioned table to the
// partitioned key build instead of refusing the plain table's USING INDEX
// sequence the planner made for it: the verdict is executed, names the
// partitions built, and the parent's constraint and both partitions' exist.
func TestRunRoutesAPartitionedPrimaryKeyToTheKeyBuild(t *testing.T) {
	pool, url, schema := partitionedOrders(t)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)", schema))

	v, err := migrate.Run(t.Context(), pool, st, keyOptions(url))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".orders", v.Table)
	assert.False(t, v.Forced)
	assert.Contains(t, v.Detail, "2 partitions, 2 built")
	assert.Equal(t, 3, constraintsPrefixed(t, pool, schema, "orders_pkey"),
		"the parent's constraint and one per partition")
}

// The build takes its table lock on a session of its own, so an unset
// Options.Connection stops the run with ErrConnectionRequired, no verdict,
// and nothing built.
func TestRunStopsBeforeThePartitionedKeyBuildWithoutAConnection(t *testing.T) {
	pool, _, schema := partitionedOrders(t)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)", schema))

	v, err := migrate.Run(t.Context(), pool, st, runOptions())
	require.ErrorIs(t, err, migrate.ErrConnectionRequired)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Zero(t, constraintsPrefixed(t, pool, schema, "orders_pkey"))
}

// A parent that already has a primary key on other columns stands in the
// way, and the build refuses before it touches a partition: a refusal with
// nothing executed, environmental because the table, not the engine, has to
// change.
func TestRunRefusesAPartitionedKeyWhenTheParentHasAPrimaryKey(t *testing.T) {
	pool, url, schema := partitionedOrders(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.orders ADD CONSTRAINT orders_old_pkey PRIMARY KEY (placed_at, id)`, schema))
	require.NoError(t, err)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)", schema))

	v, err := migrate.Run(t.Context(), pool, st, keyOptions(url))
	require.NoError(t, err, "a refusal returns no error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Zero(t, constraintsPrefixed(t, pool, schema, "orders_pkey"), "nothing was built")
}

// A table occupying the name the first partition's index needs is an
// operator's to remove; the build adds nothing to that partition and the run
// ends in a failed verdict carrying the executor's code, with the error
// returned, not a refusal: the error alone cannot prove nothing committed.
func TestRunFailsAPartitionedKeyWhoseNameIsTakenOnAPartition(t *testing.T) {
	pool, url, schema := partitionedOrders(t)
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid`, schema+".orders_2026_01").Scan(&oid))
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.orders_pkey_p%d (id int)`, schema, oid))
	require.NoError(t, err)
	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey PRIMARY KEY (id, placed_at)", schema))

	v, err := migrate.Run(t.Context(), pool, st, keyOptions(url))
	require.ErrorIs(t, err, executor.ErrPartitionedKeyNameTaken)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodePartitionedKeyNameTaken), v.Code)
	assert.Contains(t, v.Detail, "run the same statement again")
}
