package schemachange

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The LK-4 inspection decides by OID, not by name: the source's OID under
// the source name means the attempt rolled back, the shadow's OID means it
// committed, and a name borne by neither — absent, or taken by a relation
// the build never proved — is ambiguous. The same two relations are walked
// through every state so only the catalog differs between verdicts.
func TestInspectOutcomeDecidesByWhichOIDBearsTheSourceName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	source := pgx.Identifier{schema, "orders"}.Sanitize()
	shadow := pgx.Identifier{schema, ShadowName(schema, "orders")}.Sanitize()
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+source+` (id bigint PRIMARY KEY); CREATE TABLE `+shadow+` (id bigint PRIMARY KEY)`)
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	built := BuiltShadow{
		schema:    schema,
		source:    "orders",
		shadow:    ShadowName(schema, "orders"),
		sourceOID: relationOID(t, tx, schema, "orders"),
		shadowOID: relationOID(t, tx, schema, ShadowName(schema, "orders")),
	}
	require.NoError(t, tx.Rollback(t.Context()))
	inspect := func() swapOutcome {
		outcome, err := inspectOutcome(t.Context(), pool, built)
		require.NoError(t, err)
		return outcome
	}

	assert.Equal(t, outcomeNotSwapped, inspect(), "the source still bears its name")

	_, err = pool.Exec(t.Context(), `ALTER TABLE `+source+` RENAME TO `+pgx.Identifier{OldName(schema, "orders")}.Sanitize()+`; ALTER TABLE `+shadow+` RENAME TO orders`)
	require.NoError(t, err)
	assert.Equal(t, outcomeSwapped, inspect(), "the shadow bears the source's name")

	_, err = pool.Exec(t.Context(), `ALTER TABLE `+source+` RENAME TO orders_elsewhere`)
	require.NoError(t, err)
	assert.Equal(t, outcomeAmbiguous, inspect(), "nothing bears the source's name")

	_, err = pool.Exec(t.Context(), `CREATE TABLE `+source+` (impostor integer)`)
	require.NoError(t, err)
	assert.Equal(t, outcomeAmbiguous, inspect(), "a relation the build never proved bears the source's name")
}
