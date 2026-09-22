package schemachange

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// The shadow is owner-correct only because the session runs under SET LOCAL
// ROLE owner. A session that is not the owner creates a shadow the catalog
// reports as someone else's, and createShadow refuses it as ST-5 rather
// than shaping a table the source's owner does not own.
func TestCreateShadowRefusesAShadowTheSourceOwnerDoesNotOwn(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), `
		CREATE TABLE `+pgx.Identifier{schema, "widgets"}.Sanitize()+` (
			id bigint PRIMARY KEY
		)`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `ALTER TABLE `+pgx.Identifier{schema, "widgets"}.Sanitize()+` OWNER TO `+pgx.Identifier{owner}.Sanitize())
	require.NoError(t, err)
	role, err := preflight.CheckPrivileges(t.Context(), pool, schema, "widgets", preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), pool, schema, "widgets", role)
	require.NoError(t, err)

	// The transaction deliberately runs as the connected superuser, not
	// under SET LOCAL ROLE owner.
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(t.Context())) }()

	_, err = createShadow(t.Context(), tx, target, ShadowName(schema, "widgets"), owner)
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseShadowOwner, RefusalCauseOf(err))
}
