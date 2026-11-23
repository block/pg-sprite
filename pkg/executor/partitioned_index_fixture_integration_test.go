package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
)

// partitionedFixture is one subtest's view of the shared server: a pool, the
// connection target the table lock dials, and a schema of its own.
type partitionedFixture struct {
	pool   *pgxpool.Pool
	cfg    dbconn.Config
	schema string
}

// withPartitionedServer runs the subtests against one server. Each subtest
// gets its own schema, so the container start is paid once.
func withPartitionedServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) partitionedFixture)) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	run(t, func(t *testing.T) partitionedFixture {
		t.Helper()
		return partitionedFixture{pool: pool, cfg: cfg, schema: testutil.NewSchema(t, pool)}
	})
}

func (f partitionedFixture) exec(t *testing.T, format string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(format, args...))
	require.NoError(t, err)
}

// ordersByRegion creates a table partitioned by region with three leaf
// partitions that hold rows, and returns the partition names.
func (f partitionedFixture) ordersByRegion(t *testing.T) []string {
	t.Helper()
	f.exec(t, `CREATE TABLE %[1]s.orders (id bigint NOT NULL, region text NOT NULL, total int, note text) PARTITION BY LIST (region)`, f.schema)
	names := []string{"orders_eu", "orders_us", "orders_ap"}
	for _, p := range []struct{ name, region string }{{"orders_eu", "eu"}, {"orders_us", "us"}, {"orders_ap", "ap"}} {
		f.exec(t, `CREATE TABLE %[1]s.%[2]s PARTITION OF %[1]s.orders FOR VALUES IN ('%[3]s')`, f.schema, p.name, p.region)
		f.exec(t, `INSERT INTO %[1]s.orders SELECT g, '%[2]s', g %% 50, 'n' || g FROM generate_series(1, 200) g`, f.schema, p.region)
	}
	return names
}

func (f partitionedFixture) tableOID(t *testing.T, table string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT $1::regclass::oid`, pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return oid
}

// derived is the name the build gives a partition's index.
func (f partitionedFixture) derived(t *testing.T, parentIndex, partition string) string {
	t.Helper()
	return fmt.Sprintf("%s_p%d", parentIndex, f.tableOID(t, partition))
}

// build runs the build on schema.table with the statement and fails the
// test on an error.
func (f partitionedFixture) build(t *testing.T, table, sql string) executor.PartitionedIndexReport {
	t.Helper()
	rep, err := f.buildErr(t, table, sql)
	require.NoError(t, err)
	return rep
}

func (f partitionedFixture) buildErr(t *testing.T, table, sql string) (executor.PartitionedIndexReport, error) {
	t.Helper()
	return executor.BuildPartitionedIndex(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, table), sql, buildBudget)
}

// indexRow is one index of a table as the catalog reports it.
type indexRow struct {
	name    string
	kind    string
	valid   bool
	parent  string
	indexed string
}

// indexesOn lists the indexes of a table by name, with the partitioned
// index each one is attached to.
func (f partitionedFixture) indexesOn(t *testing.T, table string) map[string]indexRow {
	t.Helper()
	return f.indexesOnIn(t, f.schema, table)
}

// indexesOnIn is indexesOn for a table in any schema.
func (f partitionedFixture) indexesOnIn(t *testing.T, schema, table string) map[string]indexRow {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT c.relname, c.relkind::text, i.indisvalid, COALESCE(pc.relname, ''), pg_get_indexdef(c.oid)
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		  LEFT JOIN pg_inherits inh ON inh.inhrelid = c.oid
		  LEFT JOIN pg_class pc ON pc.oid = inh.inhparent
		 WHERE i.indrelid = $1::regclass`, pgx.Identifier{schema, table}.Sanitize())
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]indexRow{}
	for rows.Next() {
		var r indexRow
		require.NoError(t, rows.Scan(&r.name, &r.kind, &r.valid, &r.parent, &r.indexed))
		out[r.name] = r
	}
	require.NoError(t, rows.Err())
	return out
}

// requireCovered asserts the parent's index is valid and each named
// partition has exactly one index attached to it.
func (f partitionedFixture) requireCovered(t *testing.T, table, index string, partitions ...string) {
	t.Helper()
	parent, ok := f.indexesOn(t, table)[index]
	require.True(t, ok, "parent index %s must exist", index)
	assert.Equal(t, "I", parent.kind)
	assert.True(t, parent.valid, "parent index must be valid")
	for _, p := range partitions {
		var attached []string
		for _, r := range f.indexesOn(t, p) {
			if r.parent == index {
				attached = append(attached, r.name)
			}
		}
		assert.Len(t, attached, 1, "partition %s must have one index attached to %s", p, index)
	}
}

func serverVersionNum(t *testing.T, f partitionedFixture) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&n))
	return n
}

func workedTables(rep executor.PartitionedIndexReport) []string {
	var out []string
	for _, w := range rep.Worked {
		out = append(out, w.Table)
	}
	return out
}

// indexCount is how many indexes the table has, attached or not.
func (f partitionedFixture) indexCount(t *testing.T, table string) int {
	t.Helper()
	return len(f.indexesOn(t, table))
}

// attachedTo lists the table's indexes attached to the named partitioned
// index.
func (f partitionedFixture) attachedTo(t *testing.T, table, parentIndex string) []string {
	t.Helper()
	var names []string
	for name, r := range f.indexesOn(t, table) {
		if r.parent == parentIndex {
			names = append(names, name)
		}
	}
	return names
}

// requireNothingBuilt asserts the partitioned table and its partitions have
// no index at all: the build refused before it created anything.
func (f partitionedFixture) requireNothingBuilt(t *testing.T, table string, partitions ...string) {
	t.Helper()
	assert.Zero(t, f.indexCount(t, table), "the parent must have no index")
	for _, p := range partitions {
		assert.Zero(t, f.indexCount(t, p), "partition %s must have no index", p)
	}
}
