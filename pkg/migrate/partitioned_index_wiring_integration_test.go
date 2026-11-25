package migrate_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
)

// createEvents creates the live partitioned table, with one partition, in
// the pool's database and returns its schema.
func (d twoDatabases) createEvents(t *testing.T) string {
	t.Helper()
	schema := testutil.NewSchema(t, d.pool)
	_, err := d.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id int
		) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (100)`, schema))
	require.NoError(t, err)
	return schema
}

// The imperative front door proves the same wiring before the build's first
// step: an unset Options.Connection is ErrConnectionRequired and one on
// another database is the mismatch error naming Options.Connection, each with
// no verdict, and the parent carries no index afterwards.
func TestRunStopsBeforeThePartitionedIndexBuildWhenTheConnectionIsWrong(t *testing.T) {
	d := newTwoDatabases(t)
	schema := d.createEvents(t)
	st := parseOne(t, fmt.Sprintf("CREATE INDEX events_id_idx ON %s.events (id)", schema))
	indexCount := func() int {
		var n int
		require.NoError(t, d.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'events_id_idx'`, schema).Scan(&n))
		return n
	}

	v, err := migrate.Run(t.Context(), d.pool, st, runOptions())
	require.ErrorIs(t, err, migrate.ErrConnectionRequired)
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Zero(t, indexCount())

	opts := runOptions()
	opts.Connection = dbconn.Config{URL: d.otherURL}
	v, err = migrate.Run(t.Context(), d.pool, st, opts)
	mismatch, ok := errors.AsType[*migrate.PartitionedIndexConnectionMismatchError](err)
	require.True(t, ok, "want a PartitionedIndexConnectionMismatchError, got %v", err)
	assert.NotEqual(t, mismatch.PoolDatabase, mismatch.ConnectionDatabase)
	assert.Contains(t, err.Error(), "Options.Connection")
	assert.Zero(t, v, "nothing was executed, so there is no verdict")
	assert.Zero(t, indexCount())
}
