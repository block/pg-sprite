package statement_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// Two-oracle check: the name AddNotValid gives an unnamed CHECK or FOREIGN
// KEY is the name the server gives the same constraint added without one,
// into a schema where no constraint holds it yet. The rewrite then runs
// and leaves the constraint under that name, NOT VALID, ready for the
// VALIDATE step that names it.
func TestAddNotValidNamesAnUnnamedConstraintAsTheServerDoes(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	longTable := strings.Repeat("a", 60)
	longColumn := strings.Repeat("b", 60)
	wideTable := strings.Repeat("ü", 31)
	col := func(c byte) string { return strings.Repeat(string(c), 30) }
	// Each case runs in its own schema, substituted as %[1]s; table is the
	// unquoted name of the table the constraint is added to.
	cases := []struct {
		name   string
		table  string
		create string
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
				for _, ddl := range []string{
					"CREATE TABLE %[1]s.p (id int PRIMARY KEY, k int, UNIQUE (id, k))",
					"CREATE TABLE %[1]s.p3 (a int, b int, c int, UNIQUE (a, b, c))",
					tc.create,
				} {
					_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
					require.NoError(t, err, ddl)
				}
			}

			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.alter, serverSchema))
			require.NoError(t, err)
			serverName, _ := addedConstraint(t, pool, serverSchema, tc.table)

			rewritten, name, err := statement.AddNotValid(fmt.Sprintf(tc.alter, rewriteSchema))
			require.NoError(t, err)
			assert.Equal(t, serverName, name, "the name the server chose for %s", tc.alter)

			_, err = pool.Exec(t.Context(), rewritten)
			require.NoError(t, err, rewritten)
			rewrittenName, validated := addedConstraint(t, pool, rewriteSchema, tc.table)
			assert.Equal(t, name, rewrittenName)
			assert.False(t, validated, "the rewrite adds the constraint NOT VALID")
		})
	}
}

// addedConstraint returns the name and validity of the one CHECK or
// FOREIGN KEY constraint on schema.table.
func addedConstraint(t *testing.T, pool *pgxpool.Pool, schema, table string) (string, bool) {
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
	require.Len(t, found, 1)
	return found[0].name, found[0].validated
}
