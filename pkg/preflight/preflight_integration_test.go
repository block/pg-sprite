package preflight_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

func TestCheckTableUnderLimit(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.small (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.small SELECT g, 'v' FROM generate_series(1, 100) g", schema))
	require.NoError(t, err)

	pt, err := preflight.CheckTable(t.Context(), pool, schema, "small", 1<<30)
	require.NoError(t, err)
	assert.Equal(t, schema, pt.Schema())
	assert.Equal(t, "small", pt.Table())
	assert.Positive(t, pt.TotalBytes(), "a populated table must report a nonzero on-disk size")
}

func TestCheckTableOverLimit(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.big (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.big SELECT g, repeat('x', 100) FROM generate_series(1, 10000) g", schema))
	require.NoError(t, err)

	_, err = preflight.CheckTable(t.Context(), pool, schema, "big", 1)
	var sizeErr *preflight.SizeError
	require.ErrorAs(t, err, &sizeErr)
	assert.Positive(t, sizeErr.TotalBytes)
	assert.Equal(t, int64(1), sizeErr.LimitBytes)
}

// A partitioned parent's own relation is 0 bytes on disk; the guard must sum
// the partitions so a huge partitioned table cannot slip under the limit.
func TestCheckTableSumsPartitions(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.parted (id int, v text) PARTITION BY RANGE (id)", schema),
		fmt.Sprintf("CREATE TABLE %s.parted_lo PARTITION OF %s.parted FOR VALUES FROM (0) TO (5000)", schema, schema),
		fmt.Sprintf("CREATE TABLE %s.parted_hi PARTITION OF %s.parted FOR VALUES FROM (5000) TO (10001)", schema, schema),
		fmt.Sprintf("INSERT INTO %s.parted SELECT g, repeat('x', 100) FROM generate_series(0, 10000) g", schema),
	} {
		_, err = pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	pt, err := preflight.CheckTable(t.Context(), pool, schema, "parted", 1<<30)
	require.NoError(t, err)
	assert.Positive(t, pt.TotalBytes(), "the guard must see the partitions' bytes, not the parent's zero")

	_, err = preflight.CheckTable(t.Context(), pool, schema, "parted", 1)
	var sizeErr *preflight.SizeError
	require.ErrorAs(t, err, &sizeErr, "a populated partitioned table must exceed a 1-byte limit")
}

func TestCheckTableMissingTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = preflight.CheckTable(t.Context(), pool, schema, "nope", 1<<30)
	require.ErrorIs(t, err, preflight.ErrTableNotFound)
}

func TestCheckTableRefusesNonTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE VIEW %s.v AS SELECT 1 AS one", schema))
	require.NoError(t, err)

	_, err = preflight.CheckTable(t.Context(), pool, schema, "v", 1<<30)
	require.ErrorIs(t, err, preflight.ErrNotTable)
}

func TestCheckTableQuotedIdentifiers(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s."Order Items" (id int)`, schema))
	require.NoError(t, err)

	pt, err := preflight.CheckTable(t.Context(), pool, schema, "Order Items", 1<<30)
	require.NoError(t, err)
	assert.Equal(t, "Order Items", pt.Table())
}

func TestCheckTableRejectsNonPositiveLimit(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	_, err = preflight.CheckTable(t.Context(), pool, "", "whatever", 0)
	require.Error(t, err)
}
