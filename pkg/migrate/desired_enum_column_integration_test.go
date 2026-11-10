package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Enum-typed columns are ordinary columns to the desired-state path: the
// desired file names the enum unqualified, introspection renders the live
// column through format_type, and the two compare equal, so each change
// below plans exactly one native statement and a second run plans nothing.
// The enum itself is not desired-state content; it already exists in the
// target schema.

// enumDesiredFixture creates the account_state enum in a fresh schema, then
// the live table when liveTable is not empty.
func enumDesiredFixture(t *testing.T, pool *pgxpool.Pool, liveTable string) string {
	t.Helper()
	schema := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.account_state AS ENUM ('open', 'closed')`, schema))
	require.NoError(t, err)
	if liveTable != "" {
		_, err = pool.Exec(t.Context(), fmt.Sprintf(liveTable, schema))
		require.NoError(t, err)
	}
	return schema
}

// convergeEnumDesired runs the desired file, asserts it executed exactly
// wantSQL natively, and asserts a second run finds nothing left to do.
func convergeEnumDesired(t *testing.T, pool *pgxpool.Pool, schema, desiredSQL, wantSQL string) {
	t.Helper()
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, "%s: %s", res.Reason, res.Detail)
	require.Len(t, res.Plan.Statements, 1)
	assert.Equal(t, fmt.Sprintf(wantSQL, schema), res.Plan.Statements[0].SQL)

	again, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Empty(t, again.Plan.Statements, "the enum column already matches the desired file")
}

func enumPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// Adding a NOT NULL enum column with a constant default is a catalog-only
// change: PostgreSQL stores the default instead of rewriting the table.
func TestRunDesiredAddsAnEnumColumnWithADefault(t *testing.T) {
	pool := enumPool(t)
	schema := enumDesiredFixture(t, pool, `
		CREATE TABLE %s.accounts (
			id bigint PRIMARY KEY
		)`)

	convergeEnumDesired(t, pool, schema, `
		CREATE TABLE accounts (
			id    bigint PRIMARY KEY,
			state account_state NOT NULL DEFAULT 'open'
		);`,
		`ALTER TABLE %s.accounts ADD COLUMN state account_state DEFAULT ('open'::account_state) NOT NULL`)

	var state string
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(`
		SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = '%s.accounts'::regclass AND attname = 'state'`, schema)).Scan(&state))
	// The test session's search_path omits the fixture schema, so
	// format_type qualifies the name: the column is bound to that schema's enum.
	assert.Equal(t, schema+".account_state", state)
}

// An index on an enum column is planned like any other column index.
func TestRunDesiredIndexesAnEnumColumn(t *testing.T) {
	pool := enumPool(t)
	schema := enumDesiredFixture(t, pool, `
		CREATE TABLE %[1]s.accounts (
			id    bigint PRIMARY KEY,
			state %[1]s.account_state NOT NULL
		)`)

	convergeEnumDesired(t, pool, schema, `
		CREATE TABLE accounts (
			id    bigint PRIMARY KEY,
			state account_state NOT NULL
		);
		CREATE INDEX accounts_state_idx ON accounts (state);`,
		`CREATE INDEX accounts_state_idx ON %s.accounts USING btree (state)`)
}

// Changing an enum column's default renders the new default cast to the
// enum, the form introspection reads back, so the rerun is empty.
func TestRunDesiredChangesAnEnumColumnDefault(t *testing.T) {
	pool := enumPool(t)
	schema := enumDesiredFixture(t, pool, `
		CREATE TABLE %[1]s.accounts (
			id    bigint PRIMARY KEY,
			state %[1]s.account_state NOT NULL DEFAULT 'open'
		)`)

	convergeEnumDesired(t, pool, schema, `
		CREATE TABLE accounts (
			id    bigint PRIMARY KEY,
			state account_state NOT NULL DEFAULT 'closed'
		);`,
		`ALTER TABLE %s.accounts ALTER COLUMN state SET DEFAULT 'closed'::account_state`)
}

// A table that does not exist yet is created with its enum column as
// written; the greenfield create resolves the enum in the target schema.
func TestRunDesiredCreatesATableWithAnEnumColumn(t *testing.T) {
	pool := enumPool(t)
	schema := enumDesiredFixture(t, pool, "")

	convergeEnumDesired(t, pool, schema, `
		CREATE TABLE accounts (
			id    bigint PRIMARY KEY,
			state account_state NOT NULL DEFAULT 'open'
		);`,
		`CREATE TABLE %s.accounts (id bigint PRIMARY KEY, state account_state NOT NULL DEFAULT 'open')`)
}
