package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// nestedRowDesired declares every replayable kind of expression with a
// nested row comparison, which the server expands one row level each time it
// parses the text: the first build prints ROW(a, b) IS DISTINCT FROM ROW(1, 2)
// for the inner rows, and only a second parse expands those too.
const nestedRowDesired = `
CREATE TABLE t (
  a int, b int, c int, d int, e int,
  f boolean DEFAULT (((1, 2), (3, 4), 5) IS DISTINCT FROM ((1, 2), (3, 4), 6)),
  CONSTRAINT ck CHECK (((a, b), (c, d), e) IS DISTINCT FROM ((1, 2), (3, 4), 5))
);
CREATE INDEX t_p_idx ON t (a) WHERE ((a, b), (c, d), e) IS DISTINCT FROM ((1, 2), (3, 4), 5);
`

// The live table holds whatever text its definitions printed after the
// replays they went through: one when the desired file created it, more
// when it was recreated from its own exported definitions. Every text on
// that chain is the same definition, so the diff is empty against either
// table, and a real change to the definition still plans its drop and add.
func TestDiffTakesEveryReplayOfADesiredExpressionAsTheSame(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	ds, err := statement.ParseDesired(nestedRowDesired)
	require.NoError(t, err)

	t.Run("a table the desired file created", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		createFromDesired(t, pool, schema, nestedRowDesired)
		assert.Empty(t, diffDesired(t, pool, schema, ds))
	})

	t.Run("a table recreated from its own definitions", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		createFromDesired(t, pool, schema, nestedRowDesired)
		before := liveDefinitions(t, pool, schema)
		replayLiveDefinitions(t, pool, schema)
		after := liveDefinitions(t, pool, schema)
		require.NotEqual(t, before, after, "the second parse must expand the inner rows, or this case tests nothing")
		assert.Empty(t, diffDesired(t, pool, schema, ds))
		replayLiveDefinitions(t, pool, schema)
		assert.Empty(t, diffDesired(t, pool, schema, ds), "a third parse is on the same chain")
	})

	t.Run("a real change to the definition", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		createFromDesired(t, pool, schema, nestedRowDesired)
		replayLiveDefinitions(t, pool, schema)
		changed, err := statement.ParseDesired(`
			CREATE TABLE t (
			  a int, b int, c int, d int, e int,
			  f boolean DEFAULT (((1, 2), (3, 4), 5) IS DISTINCT FROM ((1, 2), (3, 4), 6)),
			  CONSTRAINT ck CHECK (((a, b), (c, d), e) IS DISTINCT FROM ((1, 2), (3, 4), 7))
			);
			CREATE INDEX t_p_idx ON t (a) WHERE ((a, b), (c, d), e) IS DISTINCT FROM ((1, 2), (3, 4), 5);`)
		require.NoError(t, err)
		var sqls []string
		for _, c := range diffDesired(t, pool, schema, changed) {
			sqls = append(sqls, c.SQL)
		}
		assert.Equal(t, []string{
			fmt.Sprintf(`ALTER TABLE "%s"."t" DROP CONSTRAINT "ck"`, schema),
			fmt.Sprintf(`ALTER TABLE "%s"."t" ADD CONSTRAINT "ck" CHECK ((((ROW(a, b) IS DISTINCT FROM ROW(1, 2)) OR (ROW(c, d) IS DISTINCT FROM ROW(3, 4))) OR (e IS DISTINCT FROM 7)))`, schema),
		}, sqls, "only the changed check is planned, with the first build's text")
	})
}

// A generation expression settles the same way on a server that can
// replace one in place.
func TestDiffTakesEveryReplayOfAGenerationExpressionAsTheSame(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	var num int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_setting('server_version_num')::int").Scan(&num))
	if num < 170000 {
		t.Skipf("ALTER COLUMN ... SET EXPRESSION exists from PostgreSQL 17; server reports %d", num)
	}
	const desired = `CREATE TABLE t (a int, b int, c int,
		g boolean GENERATED ALWAYS AS (((a, b), c) IS DISTINCT FROM ((1, 2), 3)) STORED);`
	ds, err := statement.ParseDesired(desired)
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)
	createFromDesired(t, pool, schema, desired)
	assert.Empty(t, diffDesired(t, pool, schema, ds))

	var expr string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT pg_get_expr(d.adbin, d.adrelid) FROM pg_attrdef d
		WHERE d.adrelid = $1::regclass`, schema+".t").Scan(&expr))
	_, err = pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN g SET EXPRESSION AS (%s)", schema, expr))
	require.NoError(t, err)
	assert.Empty(t, diffDesired(t, pool, schema, ds))
}

// createFromDesired creates the desired file's objects in schema, as the
// create path would.
func createFromDesired(t *testing.T, pool *pgxpool.Pool, schema, sql string) {
	t.Helper()
	ds, err := statement.ParseDesired(sql)
	require.NoError(t, err)
	for _, st := range ds.Statements() {
		qualified, err := statement.Qualify(st.SQL(), schema)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), qualified)
		require.NoError(t, err)
	}
}

// diffDesired diffs the live table t in schema against ds.
func diffDesired(t *testing.T, pool *pgxpool.Pool, schema string, ds statement.DesiredSchema) []schemadiff.Change {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	return changes
}

// liveDefinitions reads the text the server prints for t's check, default,
// and partial index, in that order.
func liveDefinitions(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	var check, def, index string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT pg_get_constraintdef(c.oid),
		       (SELECT pg_get_expr(d.adbin, d.adrelid) FROM pg_attrdef d WHERE d.adrelid = c.conrelid),
		       pg_get_indexdef(($1 || '.t_p_idx')::regclass)
		FROM pg_constraint c WHERE c.conrelid = ($1 || '.t')::regclass AND c.conname = 'ck'`, schema).
		Scan(&check, &def, &index))
	return []string{check, def, index}
}

// replayLiveDefinitions recreates t's check, default, and partial index from
// the text the server printed for them, as recreating the table from its
// own exported definitions would.
func replayLiveDefinitions(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	defs := liveDefinitions(t, pool, schema)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.t DROP CONSTRAINT ck, ADD CONSTRAINT ck %[2]s;
		ALTER TABLE %[1]s.t ALTER COLUMN f SET DEFAULT %[3]s;
		DROP INDEX %[1]s.t_p_idx;
		%[4]s`, schema, defs[0], defs[1], defs[2]))
	require.NoError(t, err)
}
