package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// An app keeps citext in an extensions schema and writes `email citext`
// unqualified. With --extension-schema naming that schema, migrate
// --desired creates the missing table, the next run finds nothing to do,
// and a column added to the file later is planned with its type qualified
// and converges. The same file without the flag stops before creating
// anything.
func TestMigrateDesiredResolvesExtensionSchema(t *testing.T) {
	url := testutil.NewDatabase(t, testutil.StartPostgres(t))
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), "CREATE SCHEMA extensions; CREATE EXTENSION citext WITH SCHEMA extensions")
	require.NoError(t, err)
	schema := testutil.NewSchema(t, pool)

	const created = `CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL
 );`
	var out strings.Builder
	require.Error(t, desiredCommand(t, url, schema, created).run(t.Context(), &out))
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound, "an unresolved type creates nothing")

	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, created, "--extension-schema", "extensions").run(t.Context(), &out))
	var result migrate.DesiredResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "extensions.citext", live.Columns[1].Type)

	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, created, "--extension-schema", "extensions").run(t.Context(), &out))
	result = migrate.DesiredResult{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	assert.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	assert.Empty(t, result.Plan.Statements, "the created table already matches the file")

	const added = `CREATE TABLE accounts (
 id int PRIMARY KEY,
 email citext NOT NULL,
 alias citext
 );`
	out.Reset()
	require.NoError(t, desiredCommand(t, url, schema, added, "--extension-schema", "extensions").run(t.Context(), &out))
	result = migrate.DesiredResult{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &result))
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome, result.Detail)
	require.Len(t, result.Plan.Statements, 1)
	assert.Equal(t, "ALTER TABLE "+schema+".accounts ADD COLUMN alias extensions.citext", result.Plan.Statements[0].SQL)
	require.NotNil(t, result.Convergence)
	live, err = schemadiff.Introspect(t.Context(), pool, schema, "accounts")
	require.NoError(t, err)
	require.Len(t, live.Columns, 3)
	assert.Equal(t, "extensions.citext", live.Columns[2].Type)
}
