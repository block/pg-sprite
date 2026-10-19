package statement_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// Two-oracle check: the name AddNotValid gives an unnamed CHECK or FOREIGN
// KEY, told the table's live columns and the constraint names its schema
// already uses, is the name the server gives the same constraint added
// without one, including when the first choice is taken and the server
// moves on to a numbered label. The rewrite then runs and leaves the
// constraint under that name, NOT VALID, ready for the VALIDATE step that
// names it.
func TestAddNotValidNamesAnUnnamedConstraintAsTheServerDoes(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	longTable := strings.Repeat("a", 60)
	longColumn := strings.Repeat("b", 60)
	wideTable := strings.Repeat("ü", 31)
	col := func(c byte) string { return strings.Repeat(string(c), 30) }
	// Each case runs in its own schema, substituted as %[1]s; table is the
	// unquoted name of the table the constraint is added to, and setup runs
	// after create, before the constraint is added.
	cases := []struct {
		name   string
		table  string
		create string
		setup  []string
		alter  string
	}{
		{
			name:   "check on one column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (qty > 0)",
		},
		{
			name:   "check on one column referenced twice",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (qty > 0 AND qty < 100)",
		},
		{
			name:   "check on two columns",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (qty > 0 OR code <> '')",
		},
		{
			name:   "check on no column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (1 > 0)",
		},
		{
			name:   "check on a column inside a function call",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (length(code) > 0)",
		},
		{
			name:   "check on a column qualified by its table",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (t.qty > 0)",
		},
		{
			name:   "check on a column qualified by its schema and table",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (%[1]s.t.qty > 0)",
		},
		{
			name:   "check on a field of a composite column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (addr %[1]s.address)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK ((addr).zip <> '')",
		},
		{
			name:   "check on a field of a composite column qualified by its table",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (addr %[1]s.address)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK ((t.addr).zip <> '')",
		},
		{
			name:   "check on the whole row by the table's bare name",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (t IS NOT NULL)",
		},
		{
			name:   "check on a field selected from the whole row",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK ((t).qty > 0)",
		},
		{
			name:   "check on a field selected from the whole row's star",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK ((t.*).qty > 0)",
		},
		{
			name:   "check on a row of the whole row's one column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (ROW(t.*) IS NOT NULL)",
		},
		{
			name:   "check on a row of the whole row's two columns",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (ROW(t.*) IS NOT NULL)",
		},
		{
			name:   "check on a row of a column and the whole row's one column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (ROW(qty, t.*) IS NOT NULL)",
		},
		{
			name:   "check on the whole row and a field selected from it",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int, code text)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (t IS NOT NULL AND (t).qty > 0)",
		},
		{
			name:   "check on a column named like its table",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (t int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (t > 0)",
		},
		{
			name:   "check whose first choice another table in the schema holds",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			setup:  []string{"CREATE TABLE %[1]s.other (x int, CONSTRAINT t_qty_check CHECK (x > 0))"},
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (qty > 0)",
		},
		{
			name:   "check whose first two choices the table and a domain hold",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			setup: []string{
				"ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty < 1000)",
				"CREATE DOMAIN %[1]s.positive AS int CONSTRAINT t_qty_check1 CHECK (VALUE > 0)",
			},
			alter: "ALTER TABLE %[1]s.t ADD CHECK (qty > 0)",
		},
		{
			name:   "check whose truncated first choice is taken",
			table:  longTable,
			create: "CREATE TABLE %[1]s." + longTable + " (" + longColumn + " int)",
			setup: []string{
				"ALTER TABLE %[1]s." + longTable + " ADD CHECK (" + longColumn + " < 1000)",
			},
			alter: "ALTER TABLE %[1]s." + longTable + " ADD CHECK (" + longColumn + " > 0)",
		},
		{
			name:   "foreign key whose first choice another table in the schema holds",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (pid int)",
			setup:  []string{"CREATE TABLE %[1]s.other (x int, CONSTRAINT t_pid_fkey CHECK (x > 0))"},
			alter:  "ALTER TABLE %[1]s.t ADD FOREIGN KEY (pid) REFERENCES %[1]s.p (id)",
		},
		{
			name:   "check on a column quoted as a star",
			table:  "t",
			create: `CREATE TABLE %[1]s.t ("*" int)`,
			alter:  `ALTER TABLE %[1]s.t ADD CHECK ("*" > 0)`,
		},
		{
			name:   "check on the whole row",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (qty int)",
			alter:  "ALTER TABLE %[1]s.t ADD CHECK (t.* IS NOT NULL)",
		},
		{
			name:   "check on a quoted mixed-case table and column",
			table:  "Orders",
			create: `CREATE TABLE %[1]s."Orders" ("Qty" int)`,
			alter:  `ALTER TABLE %[1]s."Orders" ADD CHECK ("Qty" > 0)`,
		},
		{
			name:   "check whose name is truncated",
			table:  longTable,
			create: "CREATE TABLE %[1]s." + longTable + " (" + longColumn + " int)",
			alter:  "ALTER TABLE %[1]s." + longTable + " ADD CHECK (" + longColumn + " > 0)",
		},
		{
			name:   "check whose truncation lands inside a multibyte character",
			table:  wideTable,
			create: "CREATE TABLE %[1]s." + wideTable + " (qty int)",
			alter:  "ALTER TABLE %[1]s." + wideTable + " ADD CHECK (qty > 0)",
		},
		{
			name:   "foreign key on one column",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (pid int)",
			alter:  "ALTER TABLE %[1]s.t ADD FOREIGN KEY (pid) REFERENCES %[1]s.p (id)",
		},
		{
			name:   "foreign key on two columns",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (pid int, pk int)",
			alter:  "ALTER TABLE %[1]s.t ADD FOREIGN KEY (pid, pk) REFERENCES %[1]s.p (id, k)",
		},
		{
			name:   "foreign key whose column list is truncated",
			table:  "t",
			create: "CREATE TABLE %[1]s.t (" + col('x') + " int, " + col('y') + " int, " + col('z') + " int)",
			alter: "ALTER TABLE %[1]s.t ADD FOREIGN KEY (" + col('x') + ", " + col('y') + ", " + col('z') + ") " +
				"REFERENCES %[1]s.p3 (a, b, c)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverSchema := testutil.NewSchema(t, pool)
			rewriteSchema := testutil.NewSchema(t, pool)
			for _, schema := range []string{serverSchema, rewriteSchema} {
				ddls := []string{
					"CREATE TABLE %[1]s.p (id int PRIMARY KEY, k int, UNIQUE (id, k))",
					"CREATE TABLE %[1]s.p3 (a int, b int, c int, UNIQUE (a, b, c))",
					"CREATE TYPE %[1]s.address AS (zip text)",
					tc.create,
				}
				for _, ddl := range append(ddls, tc.setup...) {
					_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
					require.NoError(t, err, ddl)
				}
			}

			before := tableConstraints(t, pool, serverSchema, tc.table)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.alter, serverSchema))
			require.NoError(t, err)
			serverName, _ := addedConstraint(t, pool, serverSchema, tc.table, before)

			taken, err := preflight.SchemaConstraintNames(t.Context(), pool, rewriteSchema)
			require.NoError(t, err)
			naming := statement.ConstraintNaming{Columns: tableColumns(t, pool, rewriteSchema, tc.table), Taken: taken}
			rewritten, name, err := statement.AddNotValid(fmt.Sprintf(tc.alter, rewriteSchema), naming)
			require.NoError(t, err)
			assert.Equal(t, serverName, name, "the name the server chose for %s", tc.alter)

			before = tableConstraints(t, pool, rewriteSchema, tc.table)
			_, err = pool.Exec(t.Context(), rewritten)
			require.NoError(t, err, rewritten)
			rewrittenName, validated := addedConstraint(t, pool, rewriteSchema, tc.table, before)
			assert.Equal(t, name, rewrittenName)
			assert.False(t, validated, "the rewrite adds the constraint NOT VALID")
		})
	}
}

// Without the schema's constraint names the rewrite states the server's
// first choice, and when the table already holds it the NOT VALID step
// fails with a duplicate-object error before anything is added.
func TestAddNotValidUnawareOfATakenNameFailsBeforeAdding(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (qty int)",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty < 1000)",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}

	rewritten, name, err := statement.AddNotValid(fmt.Sprintf("ALTER TABLE %s.t ADD CHECK (qty > 0)", schema), statement.ConstraintNaming{})
	require.NoError(t, err)
	assert.Equal(t, "t_qty_check", name)
	_, err = pool.Exec(t.Context(), rewritten)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42710", pgErr.Code, "duplicate_object")
	assert.Equal(t, map[string]bool{"t_qty_check": true}, tableConstraints(t, pool, schema, "t"), "nothing was added")
}

// tableConstraints returns the names of the CHECK and FOREIGN KEY
// constraints on schema.table.
func tableConstraints(t *testing.T, pool *pgxpool.Pool, schema, table string) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT conname::text FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype IN ('c', 'f')`, pgx.Identifier{schema, table}.Sanitize())
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// tableColumns returns the live columns of schema.table.
func tableColumns(t *testing.T, pool *pgxpool.Pool, schema, table string) map[string]bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT attname::text FROM pg_catalog.pg_attribute
		WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped`, pgx.Identifier{schema, table}.Sanitize())
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	columns := make(map[string]bool, len(names))
	for _, name := range names {
		columns[name] = true
	}
	return columns
}

// addedConstraint returns the name and validity of the one CHECK or
// FOREIGN KEY constraint on schema.table that before does not name.
func addedConstraint(t *testing.T, pool *pgxpool.Pool, schema, table string, before map[string]bool) (string, bool) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT conname, convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype IN ('c', 'f')`, pgx.Identifier{schema, table}.Sanitize())
	require.NoError(t, err)
	type constraint struct {
		name      string
		validated bool
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (constraint, error) {
		var c constraint
		return c, row.Scan(&c.name, &c.validated)
	})
	require.NoError(t, err)
	found = slices.DeleteFunc(found, func(c constraint) bool { return before[c.name] })
	require.Len(t, found, 1)
	return found[0].name, found[0].validated
}
