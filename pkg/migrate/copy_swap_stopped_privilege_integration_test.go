package migrate_test

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A database where one role once ran a copy-and-swap carries the engine's
// checkpoint store, which another role that owns its own table may have no
// privilege to read. That role's statements route as they would with no
// stopped run: a native statement it may run executes, and one it may not
// is refused by the privilege preflight that names what it lacks, never by
// the read of a store it cannot see.
func TestRunRoutesAStatementOfARoleThatCannotReadTheCheckpointStore(t *testing.T) {
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
	_, err = admin.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.owned (
		    id int PRIMARY KEY
		);
		ALTER TABLE %[1]s.owned OWNER TO %[2]s;
		CREATE TABLE %[1]s.not_owned (
		    id int PRIMARY KEY
		);
		GRANT USAGE ON SCHEMA %[1]s TO %[2]s`,
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{tableOwner}.Sanitize()))
	require.NoError(t, err)
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	u.User = url.UserPassword(tableOwner, password)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

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
