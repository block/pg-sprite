// White-box tests for the partitioned index build's body: the refusals the
// public entry point cannot reach on demand, run directly against a pool.
// The build itself is tested through BuildPartitionedIndex.

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
	return f.runSQL(t, index, fmt.Sprintf("CREATE INDEX %s ON %s.orders (total)", index, f.schema))
}

// runSQL runs the build for index with the statement sql.
func (f runFixture) runSQL(t *testing.T, index, sql string) (PartitionedIndexReport, error) {
	t.Helper()
	req := partitionedIndexRequest{schema: f.schema, table: "orders", index: index}
	return runPartitionedIndex(t.Context(), f.pool, req, sql, ConcurrentBudget{Overall: time.Minute}, nil, 0)
}

// builtFlow runs the build for index to completion and returns a flow that
// has read the table, its partition tree, and the parent's index, so a test
// can run one of the flow's steps on its own against the finished build.
func (f runFixture) builtFlow(t *testing.T, index string) (*partitionedIndexFlow, []partitionNode) {
	t.Helper()
	_, err := f.run(t, index)
	require.NoError(t, err)
	fl := &partitionedIndexFlow{
		tx: briefTx{pool: f.pool}, pool: f.pool, budget: ConcurrentBudget{Overall: time.Minute},
		req: partitionedIndexRequest{schema: f.schema, table: "orders", index: index},
	}
	require.NoError(t, fl.resolveTarget(t.Context()))
	tree, err := listPartitionTree(t.Context(), f.pool, fl.target.tableOID)
	require.NoError(t, err)
	parent, found, err := relationByName(t.Context(), f.pool, f.schema, index)
	require.NoError(t, err)
	require.True(t, found)
	fl.parent = parent
	return fl, tree
}

// node returns the tree's entry for the partition named table.
func node(t *testing.T, tree []partitionNode, table string) partitionNode {
	t.Helper()
	for _, n := range tree {
		if n.name == table {
			return n
		}
	}
	require.Failf(t, "partition not listed", "%s is not in the tree", table)
	return partitionNode{}
}

// invalidate marks the index named index invalid in the catalog, the state
// a failed build or a server fault leaves behind.
func (f runFixture) invalidate(t *testing.T, index string) {
	t.Helper()
	f.exec(t, `UPDATE pg_catalog.pg_index SET indisvalid = false WHERE indexrelid = '%s.%s'::regclass`, f.schema, index)
}

// A partition's failed build surfaces through the run with the partition
// named and the build's own verdict kept: unique rows that are not unique
// are a data fault. The parent's index stays invalid, so it never reports
// an index that covers only some partitions.
func TestRunPartitionedIndexSurfacesALeafBuildFailure(t *testing.T) {
	f := newRunFixture(t)

	_, err := f.runSQL(t, "idx_orders_region", fmt.Sprintf("CREATE UNIQUE INDEX idx_orders_region ON %s.orders (region)", f.schema))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build the index of partition "+f.schema+".orders_")
	assert.Equal(t, CodeDataFault, OutcomeCode(err))
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`, f.schema+".idx_orders_region").Scan(&valid))
	assert.False(t, valid, "the parent's index stays invalid")
}

// An index attached to the parent's index that the server holds invalid
// is refused by name: it belongs to the parent's index, so the build never
// builds over it or drops it, and the parent cannot become valid until an
// operator repairs it.
func TestRunPartitionedIndexNamesAnInvalidAttachedIndex(t *testing.T) {
	f := newRunFixture(t)
	fl, tree := f.builtFlow(t, "idx_orders_total")
	us := node(t, tree, "orders_us")
	broken := derivedPartitionIndexName("idx_orders_total", us.oid)
	f.invalidate(t, broken)

	attached, err := fl.alreadyAttached(t.Context(), us, fl.parent)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.False(t, attached)
	assert.Contains(t, err.Error(), f.schema+".orders_us")
	assert.Contains(t, err.Error(), f.schema+"."+broken)
	assert.Contains(t, err.Error(), "is invalid")
}

// A built index that is not on the partition the tree listed is refused:
// the partition was replaced under its name while the build ran.
func TestRunPartitionedIndexRefusesAnIndexBuiltOnAnotherTable(t *testing.T) {
	f := newRunFixture(t)
	fl, tree := f.builtFlow(t, "idx_orders_total")
	us := node(t, tree, "orders_us")
	eu := node(t, tree, "orders_eu")
	onEU := derivedPartitionIndexName("idx_orders_total", eu.oid)
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT $1::regclass::oid`, f.schema+"."+onEU).Scan(&oid))

	_, err := fl.readBuiltIndex(t.Context(), us, onEU, oid)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), f.schema+".orders_us")
	assert.Contains(t, err.Error(), "was replaced while the build ran")
}

// The build reports success only when the parent's index is valid when it
// is read back. A parent left invalid after every listed partition has an
// attached index is refused as an invariant violation, not reported built.
func TestRunPartitionedIndexVerifyRefusesAnInvalidParent(t *testing.T) {
	f := newRunFixture(t)
	fl, _ := f.builtFlow(t, "idx_orders_total")
	f.invalidate(t, "idx_orders_total")

	err := fl.verify(t.Context())

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), f.schema+".idx_orders_total is invalid")
}
