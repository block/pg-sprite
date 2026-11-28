package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/verdict"
)

const narrowedPrimaryKeyDesired = `CREATE TABLE accounts (id bigint PRIMARY KEY, name text);`

// narrowedPrimaryKeyTable creates accounts with a primary key on (id, name)
// holding rows, and returns the reviewed plan that narrows the key to (id)
// and lets name go NULL.
func narrowedPrimaryKeyTable(t *testing.T, rows string) (*pgxpool.Pool, string, plan.Report) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.accounts (id bigint, name text, PRIMARY KEY (id, name));
		INSERT INTO %[1]s.accounts VALUES %[2]s`, schema, rows))
	require.NoError(t, err)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, narrowedPrimaryKeyDesired)})
	require.NoError(t, err)
	return pool, schema, report
}

func runNarrowedPrimaryKey(t *testing.T, pool *pgxpool.Pool, schema string, report plan.Report) (migrate.DesiredResult, error) {
	t.Helper()
	return migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, narrowedPrimaryKeyDesired),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    schema + ".accounts",
	}, runOptions())
}

// primaryKeyDef returns the live definition of accounts' primary key, or ""
// when the table has none.
func primaryKeyDef(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	var def string
	err := pool.QueryRow(t.Context(), `
		SELECT coalesce(max(pg_get_constraintdef(con.oid)), '')
		FROM pg_constraint con
		WHERE con.conrelid = format('%I.accounts', $1::text)::regclass AND con.contype = 'p'`, schema).Scan(&def)
	require.NoError(t, err)
	return def
}

// A desired file narrows accounts' primary key from (id, name) to (id) and
// lets name go NULL. The plan builds the new key's index beside the old key,
// swaps it in, and only then drops name's NOT NULL, which the old key owned.
// While two rows share id 7, the build fails and the old key stays: the
// table keeps rejecting duplicates of (id, name), and name stays NOT NULL.
func TestRunDesiredNarrowedPrimaryKeyOverDuplicateRowsKeepsTheOldKey(t *testing.T) {
	pool, schema, report := narrowedPrimaryKeyTable(t, `(7, 'a'), (7, 'b')`)
	require.Len(t, report.Statements, 3)
	assert.Contains(t, report.Statements[0].SQL, "CREATE UNIQUE INDEX accounts_pkey_new")
	assert.Contains(t, report.Statements[1].SQL, "USING INDEX accounts_pkey_new")
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.accounts ALTER COLUMN name DROP NOT NULL", schema), report.Statements[2].SQL,
		"name's NOT NULL is dropped once the old key no longer owns it")

	res, err := runNarrowedPrimaryKey(t, pool, schema, report)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "23505")
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Equal(t, "PRIMARY KEY (id, name)", primaryKeyDef(t, pool, schema), "the old key still rejects duplicates")
	var nullable string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'accounts' AND column_name = 'name'`, schema).Scan(&nullable))
	assert.Equal(t, "NO", nullable)
}

// With no duplicate ids, the same plan narrows the key and name becomes
// nullable. A fresh plan is empty.
func TestRunDesiredNarrowsAPrimaryKeyAndThenDropsTheNotNullItOwned(t *testing.T) {
	pool, schema, report := narrowedPrimaryKeyTable(t, `(7, 'a'), (8, 'b')`)
	res, err := runNarrowedPrimaryKey(t, pool, schema, report)
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s", res.Detail)
	assert.Equal(t, "PRIMARY KEY (id)", primaryKeyDef(t, pool, schema))
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.accounts VALUES (9, NULL)`, schema))
	require.NoError(t, err, "name accepts NULL")
	again, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, narrowedPrimaryKeyDesired)})
	require.NoError(t, err)
	assert.Empty(t, again.Statements)
}
