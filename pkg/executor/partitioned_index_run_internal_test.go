// White-box tests for the partitioned index build's body. The public entry
// point is not part of this layer, so the body runs directly against a pool.

package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// runFixture is a pool and a schema holding orders, a table partitioned by
// region with three leaf partitions that hold rows.
type runFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newRunFixture(t *testing.T) runFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := runFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `CREATE TABLE %s.orders (id bigint NOT NULL, region text NOT NULL, total int) PARTITION BY LIST (region)`, f.schema)
	for _, region := range []string{"eu", "us", "ap"} {
		f.exec(t, `CREATE TABLE %[1]s.orders_%[2]s PARTITION OF %[1]s.orders FOR VALUES IN ('%[2]s')`, f.schema, region)
		f.exec(t, `INSERT INTO %s.orders SELECT g, '%s', g %% 50 FROM generate_series(1, 100) g`, f.schema, region)
	}
	return f
}

func (f runFixture) exec(t *testing.T, format string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(format, args...))
	require.NoError(t, err)
}

func (f runFixture) run(t *testing.T, index string) (PartitionedIndexReport, error) {
	t.Helper()
	req := partitionedIndexRequest{schema: f.schema, table: "orders", index: index}
	sql := fmt.Sprintf("CREATE INDEX %s ON %s.orders (total)", index, f.schema)
	return runPartitionedIndex(t.Context(), f.pool, req, sql, ConcurrentBudget{Overall: time.Minute}, nil)
}

// attachedCount is how many of the table's indexes are attached to the
// partitioned index named parent.
func (f runFixture) attachedCount(t *testing.T, table, parent string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM pg_inherits h
		  JOIN pg_index i ON i.indexrelid = h.inhrelid
		 WHERE i.indrelid = $1::regclass AND h.inhparent = $2::regclass`,
		f.schema+"."+table, f.schema+"."+parent).Scan(&n))
	return n
}

// A run creates the parent's index without building it, builds each leaf's
// index concurrently under its derived name, attaches each, and reports the
// parent only once it is valid.
func TestRunPartitionedIndexBuildsAndAttachesEveryLeaf(t *testing.T) {
	f := newRunFixture(t)

	rep, err := f.run(t, "idx_orders_total")

	require.NoError(t, err)
	assert.Equal(t, f.schema, rep.Schema)
	assert.Equal(t, "idx_orders_total", rep.Index)
	assert.True(t, rep.ParentCreated)
	assert.Equal(t, 3, rep.Partitions)
	assert.NotZero(t, rep.IndexOID)
	assert.NotEmpty(t, rep.ServerVersion)
	require.Len(t, rep.Worked, 3)
	for _, w := range rep.Worked {
		assert.Equal(t, PartitionIndexBuilt, w.Action)
		assert.Equal(t, f.schema, w.Schema)
		assert.Regexp(t, `^idx_orders_total_p\d+$`, w.Index)
		assert.NotZero(t, w.IndexOID)
		assert.Equal(t, 1, f.attachedCount(t, w.Table, "idx_orders_total"), w.Table)
	}
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`, f.schema+".idx_orders_total").Scan(&valid))
	assert.True(t, valid)
}

// A relation that holds a name the build needs is left as it is, and nothing is
// attached to the parent's index.
func TestRunPartitionedIndexRefusesANameAnotherRelationHolds(t *testing.T) {
	t.Run("the parent index's name", func(t *testing.T) {
		f := newRunFixture(t)
		f.exec(t, `CREATE TABLE %s.idx_orders_total (x int)`, f.schema)

		_, err := f.run(t, "idx_orders_total")

		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
		assert.Contains(t, err.Error(), f.schema+".idx_orders_total")
	})
	t.Run("a partition index's derived name", func(t *testing.T) {
		f := newRunFixture(t)
		var oid uint32
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`, f.schema+".orders_eu").Scan(&oid))
		f.exec(t, `CREATE SEQUENCE %s.%s`, f.schema, derivedPartitionIndexName("idx_orders_total", oid))

		_, err := f.run(t, "idx_orders_total")

		require.ErrorIs(t, err, ErrPartitionedIndexNameTaken)
		assert.Contains(t, err.Error(), "a sequence")
		assert.Zero(t, f.attachedCount(t, "orders_eu", "idx_orders_total"))
	})
}
