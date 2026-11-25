package migrate_test

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A database where one role once ran a copy-and-swap carries the engine's
// checkpoint store, which another role that owns its own table may have no
// privilege to read. That role's statements route as they would with no
// stopped run: a native statement it may run executes, and one it may not
// is refused by the privilege preflight that names what it lacks, never by
// the read of a store it cannot see.
func TestRunRoutesAStatementOfARoleThatCannotReadTheCheckpointStore(t *testing.T) {
	_, pool, schema := storeBlindTableOwner(t)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.owned ADD COLUMN age int", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)

	v, err = migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.not_owned ADD COLUMN age int", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
}

// The same role, on a table where another role's copy-and-swap run stands,
// cannot read the run's row, but the catalog shows what the run left: here
// the shadow under the name the engine derives for owned. The native
//
//	ALTER TABLE owned ADD COLUMN age int
//
// would change the source under that run, so it is refused with
// copy-and-swap-run-stopped and owned keeps its columns. The shadow stands
// in for a stopped run's; what the front door reads, unable to read the
// row, is only the catalog.
func TestRunRefusesAStatementOfARoleThatCannotReadTheStoppedRunsRow(t *testing.T) {
	admin, pool, schema := storeBlindTableOwner(t)
	_, err := admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s (
		    id int PRIMARY KEY
		)`, pgx.Identifier{schema, schemachange.ShadowName(schema, "owned")}.Sanitize()))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.owned ADD COLUMN age int", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, v.Cause)
	var columns int
	require.NoError(t, admin.QueryRow(t.Context(), `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'owned'`, schema).Scan(&columns))
	assert.Equal(t, 1, columns, "nothing was executed")
}

// storeBlindTableOwner starts a server whose checkpoint store the admin
// role owns, and a login role that owns the table owned in a new schema
// but may not read the store; not_owned is the admin's. It returns the
// admin's pool, the table owner's pool, and the schema.
func storeBlindTableOwner(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool, string) {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	store, err := checkpoint.NewStore(admin, checkpoint.Options{})
	require.NoError(t, err)
	require.NoError(t, store.Ensure(t.Context()), "the store exists, owned by the admin role")

	const password = "store-blind-password"
	tableOwner := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	schema := testutil.NewSchema(t, admin)
	// The admin connection is not a superuser on every fixture (Supabase's
	// postgres role is not), so OWNER TO needs the admin to be a member of
	// the new owner and the new owner to hold CREATE on the schema. The
	// grantee is spelled by name: the Supabase image crashes on
	// GRANT ... TO CURRENT_USER.
	var adminRole string
	require.NoError(t, admin.QueryRow(t.Context(), "SELECT current_user").Scan(&adminRole))
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		GRANT %[2]s TO %[3]s;
		GRANT USAGE, CREATE ON SCHEMA %[1]s TO %[2]s;
		CREATE TABLE %[1]s.owned (
		    id int PRIMARY KEY
		);
		ALTER TABLE %[1]s.owned OWNER TO %[2]s;
		CREATE TABLE %[1]s.not_owned (
		    id int PRIMARY KEY
		)`,
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{tableOwner}.Sanitize(), pgx.Identifier{adminRole}.Sanitize()))
	require.NoError(t, err)
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	u.User = url.UserPassword(tableOwner, password)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return admin, pool, schema
}
