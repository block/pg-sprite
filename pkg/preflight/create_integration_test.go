package preflight_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The off-ladder create check, walked grant by grant: each missing access
// is a typed refusal naming the exact provisioning statement whose grantee
// is the engine role itself, and applying exactly that statement unlocks
// the next rung.
func TestCheckCreatePrivilegesWalksTheGrants(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := testutil.NewSchema(t, admin)

	const password = "create-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	engine := connectAs(t, serverURL, role, password)
	ctx := t.Context()

	// No USAGE on the schema yet.
	_, err = preflight.CheckCreatePrivileges(ctx, engine, schema)
	var privErr *preflight.PrivilegeError
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierConnect, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()), privErr.Grant)

	_, err = admin.Exec(ctx, privErr.Grant)
	require.NoError(t, err)

	// USAGE held, CREATE still missing: the off-ladder tier, and the
	// grantee is the connected role — no owner exists to inherit from.
	_, err = preflight.CheckCreatePrivileges(ctx, engine, schema)
	require.ErrorAs(t, err, &privErr)
	assert.Equal(t, preflight.TierCreateTable, privErr.Tier)
	assert.Equal(t, fmt.Sprintf("GRANT CREATE ON SCHEMA %s TO %s",
		pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()), privErr.Grant)
	assert.Empty(t, privErr.Hint, "the grant speaks for itself: its grantee is the role the check names")

	_, err = admin.Exec(ctx, privErr.Grant)
	require.NoError(t, err)

	proof, err := preflight.CheckCreatePrivileges(ctx, engine, schema)
	require.NoError(t, err)
	assert.Equal(t, role, proof.Role())
	assert.Equal(t, schema, proof.Schema())
}

func TestCheckCreatePrivilegesRefusesMissingSchema(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = preflight.CheckCreatePrivileges(t.Context(), pool, "no_such_schema")
	assert.ErrorIs(t, err, preflight.ErrSchemaNotFound)
}

// An empty schema resolves the session's creation schema — the schema an
// unqualified CREATE TABLE would land in — and the proof carries it.
func TestCheckCreatePrivilegesResolvesUnqualifiedSchema(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var creationSchema string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&creationSchema))

	proof, err := preflight.CheckCreatePrivileges(t.Context(), pool, "")
	require.NoError(t, err)
	assert.Equal(t, creationSchema, proof.Schema())
}

// A session whose search_path names no schema has no creation target for
// an unqualified check; the check fails rather than guessing a schema.
func TestCheckCreatePrivilegesRefusesEmptySearchPath(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	const password = "create-test-password"
	role := testutil.NewRole(t, admin, "LOGIN PASSWORD '"+password+"'")
	_, err = admin.Exec(t.Context(), fmt.Sprintf("ALTER ROLE %s SET search_path = ''",
		pgx.Identifier{role}.Sanitize()))
	require.NoError(t, err)

	engine := connectAs(t, serverURL, role, password)
	_, err = preflight.CheckCreatePrivileges(t.Context(), engine, "")
	assert.ErrorIs(t, err, preflight.ErrNoCreationSchema)
	assert.NotErrorIs(t, err, preflight.ErrSchemaNotFound,
		"an unresolvable search_path is not a missing schema — there is no schema name to report missing")
}
