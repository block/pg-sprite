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

// partitionedEvents creates accounts and events partitioned by created_at,
// with two partitions whose rows all reference an account, and returns the
// pool, the connection URL, and the schema. It skips on PostgreSQL 18 and
// later, where the foreign key takes the ordinary NOT VALID then VALIDATE
// route rather than the partitioned build.
func partitionedEvents(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	if major >= 18 {
		t.Skipf("PostgreSQL %d accepts NOT VALID on a partitioned table, so the foreign key takes the ordinary route", major)
	}
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.accounts (
		    id bigint PRIMARY KEY
		);
		INSERT INTO %[1]s.accounts (id)
		SELECT g FROM generate_series(1, 50) g;
		CREATE TABLE %[1]s.events (
		    id         bigint NOT NULL,
		    created_at date NOT NULL,
		    account_id bigint
		) PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
		CREATE TABLE %[1]s.events_2026_02 PARTITION OF %[1]s.events
		    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
		INSERT INTO %[1]s.events (id, created_at, account_id)
		SELECT g, DATE '2026-01-01' + (g %% 59), 1 + (g %% 50)
		  FROM generate_series(1, 500) g`, schema))
	require.NoError(t, err)
	return pool, url, schema
}

// foreignKeysIn counts the foreign keys on every table of schema.
func foreignKeysIn(t *testing.T, pool *pgxpool.Pool, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relnamespace = $1::regnamespace
		   AND c.contype = 'f'`, schema).Scan(&n))
	return n
}

// fkOptions is runOptions wired with the connection the build takes its
// table lock from.
func fkOptions(url string) migrate.Options {
	opts := runOptions()
	opts.Connection = dbconn.Config{URL: url}
	return opts
}

// migrate.Run sends an ADD FOREIGN KEY on a partitioned table to the
// partitioned foreign key build instead of refusing the plain table's NOT
// VALID then VALIDATE the planner made for it: the verdict is executed,
// names the partitions added, and the parent's foreign key and both
// partitions' exist.
func TestRunRoutesAPartitionedForeignKeyToTheBuild(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	st := parseOne(t, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))

	v, err := migrate.Run(t.Context(), pool, st, fkOptions(url))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, schema+".events", v.Table)
	assert.False(t, v.Forced)
	assert.Contains(t, v.Detail, "2 partitions, 0 found already validated, 0 validated, 2 added")
	assert.Equal(t, 3, foreignKeysIn(t, pool, schema), "the parent's foreign key and one per partition")
}

// The build takes its table lock on a session of its own, so an unset
// Options.Connection stops the run with ErrConnectionRequired, no verdict,
// and nothing added.
func TestRunStopsBeforeThePartitionedForeignKeyBuildWithoutAConnection(t *testing.T) {
	pool, _, schema := partitionedEvents(t)
	st := parseOne(t, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))

	v, err := migrate.Run(t.Context(), pool, st, runOptions())
	require.ErrorIs(t, err, migrate.ErrConnectionRequired)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Zero(t, foreignKeysIn(t, pool, schema))
}

// A parent that already has the requested foreign key under another name
// stands in the way, and the build refuses before it touches a partition: a
// refusal with nothing executed, environmental because the table, not the
// engine, has to change.
func TestRunRefusesAPartitionedForeignKeyTheParentHasUnderAnotherName(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_old_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))
	require.NoError(t, err)
	before := foreignKeysIn(t, pool, schema)
	st := parseOne(t, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))

	v, err := migrate.Run(t.Context(), pool, st, fkOptions(url))
	require.NoError(t, err, "a refusal returns no error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, v.Class)
	assert.Contains(t, v.Detail, "events_account_old_fk")
	assert.Equal(t, before, foreignKeysIn(t, pool, schema), "nothing was added")
}

// A partitioned referenced table would have the parent's statement clone the
// foreign key onto every referenced partition, which the build neither
// proves nor reads back, so it refuses before it touches a partition: a
// capability boundary.
func TestRunRefusesAPartitionedForeignKeyToAPartitionedTable(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.regions (
		    id bigint NOT NULL,
		    PRIMARY KEY (id)
		) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.regions_low PARTITION OF %[1]s.regions
		    FOR VALUES FROM (0) TO (1000)`, schema))
	require.NoError(t, err)
	st := parseOne(t, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_region_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.regions (id)`, schema))

	v, err := migrate.Run(t.Context(), pool, st, fkOptions(url))
	require.NoError(t, err, "a refusal returns no error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Zero(t, foreignKeysIn(t, pool, schema), "nothing was added")
}

// A check constraint holding the name the first partition's foreign key
// needs is an operator's to remove; the build adds nothing to that partition
// and the run ends in a failed verdict carrying the executor's code, with
// the error returned, not a refusal: the error alone cannot prove nothing
// committed.
func TestRunFailsAPartitionedForeignKeyWhoseNameIsTakenOnAPartition(t *testing.T) {
	pool, url, schema := partitionedEvents(t)
	var oid uint32
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT to_regclass($1)::oid`, schema+".events_2026_01").Scan(&oid))
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %s.events_2026_01
		    ADD CONSTRAINT events_account_fk_p%d CHECK (id > 0)`, schema, oid))
	require.NoError(t, err)
	st := parseOne(t, fmt.Sprintf(`
		ALTER TABLE %[1]s.events
		    ADD CONSTRAINT events_account_fk
		    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`, schema))

	v, err := migrate.Run(t.Context(), pool, st, fkOptions(url))
	require.ErrorIs(t, err, executor.ErrPartitionedForeignKeyNameTaken)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodePartitionedForeignKeyNameTaken), v.Code)
	assert.Contains(t, v.Detail, "run the same statement again")
	assert.Zero(t, foreignKeysIn(t, pool, schema), "nothing was added")
}
