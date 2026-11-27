package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file narrows accounts' primary key from (id, name) to (id) and
// lets name go NULL, while two rows share id 7. The key owns name's NOT NULL,
// so the plan drops the old key before it builds the new one, and the run
// needs the destructive acknowledgement. Acknowledged, it drops the old key
// and then fails to build the new one over the duplicate rows: the table is
// left with no primary key and the failed build's invalid index, and the
// result names the statements that committed before the failure.
func TestRunDesiredNarrowedPrimaryKeyOverDuplicateRowsLeavesNoKey(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.accounts (id bigint, name text, PRIMARY KEY (id, name));
		INSERT INTO %[1]s.accounts VALUES (7, 'a'), (7, 'b')`, schema))
	require.NoError(t, err)
	const desired = `CREATE TABLE accounts (id bigint PRIMARY KEY, name text);`

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, desired)})
	require.NoError(t, err)
	require.Len(t, report.Statements, 3)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.accounts DROP CONSTRAINT accounts_pkey", schema), report.Statements[0].SQL,
		"the old key is dropped before name's NOT NULL, which it owns")

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema:              schema,
		Desired:             parseDesired(t, desired),
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    schema + ".accounts",
	}, runOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SQLSTATE 23505")
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Contains(t, res.Detail, "planned statement 3 of 3 failed; the 2 preceding statements committed and remain in effect")

	var constraints int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = rel.relnamespace
		WHERE n.nspname = $1 AND rel.relname = 'accounts'`, schema).Scan(&constraints))
	assert.Zero(t, constraints, "the old key was dropped and the new one was never added")
	var valid bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'accounts_pkey'`, schema).Scan(&valid))
	assert.False(t, valid, "the failed build leaves its invalid index for the operator to drop")
}
