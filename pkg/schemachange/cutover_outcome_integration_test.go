package schemachange

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The LK-4 read decides by OID, not by name: the source's OID under the
// source name means the attempt rolled back, the shadow's OID means it
// committed and the proof is minted from the two relations, and a name
// borne by neither — absent, or taken by a relation the build never
// proved — is ambiguous. A free _old name is dropped only when the
// source's OID is gone from the catalog; a source renamed away from it is
// refused. The same two relations are walked through every state so only
// the catalog differs between verdicts.
func TestReadOutcomeDecidesByWhichOIDBearsTheSourceName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	source := pgx.Identifier{schema, "orders"}.Sanitize()
	shadow := pgx.Identifier{schema, ShadowName(schema, "orders")}.Sanitize()
	old := pgx.Identifier{schema, OldName(schema, "orders")}.Sanitize()
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+source+` (id bigint PRIMARY KEY); CREATE TABLE `+shadow+` (id bigint PRIMARY KEY)`)
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	var owner string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT current_user`).Scan(&owner))
	expected := Proof{
		Schema:      schema,
		SourceTable: "orders",
		ShadowTable: ShadowName(schema, "orders"),
		SourceOID:   relationOID(t, tx, schema, "orders"),
		ShadowOID:   relationOID(t, tx, schema, ShadowName(schema, "orders")),
		Fidelity:    FidelitySnapshot{Owner: owner},
	}
	require.NoError(t, tx.Rollback(t.Context()))
	read := func() (SwappedTable, error) {
		return readOutcome(t.Context(), pool, expected)
	}

	_, err = read()
	assert.ErrorIs(t, err, ErrNotSwapped, "the source still bears its name")

	_, err = pool.Exec(t.Context(), `ALTER TABLE `+source+` RENAME TO `+pgx.Identifier{OldName(schema, "orders")}.Sanitize()+`; ALTER TABLE `+shadow+` RENAME TO orders`)
	require.NoError(t, err)
	swapped, err := read()
	require.NoError(t, err, "the shadow bears the source's name")
	assert.Equal(t, expected.ShadowOID, swapped.LiveOID())
	assert.Equal(t, expected.SourceOID, swapped.OldOID())
	assert.Equal(t, 0, swapped.Attempts(), "a read after the fact cannot know how many acquisitions the swap needed")

	_, err = pool.Exec(t.Context(), `ALTER TABLE `+old+` RENAME TO orders_retained`)
	require.NoError(t, err)
	_, err = read()
	assert.Equal(t, CauseRelationReplaced, RefusalCauseOf(err), "nothing bears the _old name, but the retained source still exists under another")

	_, err = pool.Exec(t.Context(), `DROP TABLE `+pgx.Identifier{schema, "orders_retained"}.Sanitize())
	require.NoError(t, err)
	_, err = read()
	assert.ErrorIs(t, err, ErrOldTableNotFound, "nothing bears the _old name and the retained source is gone")

	_, err = pool.Exec(t.Context(), `CREATE TABLE `+old+` (impostor integer)`)
	require.NoError(t, err)
	_, err = read()
	assert.Equal(t, CauseRelationReplaced, RefusalCauseOf(err), "a relation the swap never retained bears the _old name")

	_, err = pool.Exec(t.Context(), `ALTER TABLE `+source+` RENAME TO orders_elsewhere`)
	require.NoError(t, err)
	_, err = read()
	assert.Equal(t, CauseOutcomeAmbiguous, RefusalCauseOf(err), "nothing bears the source's name")

	_, err = pool.Exec(t.Context(), `CREATE TABLE `+source+` (impostor integer)`)
	require.NoError(t, err)
	_, err = read()
	assert.Equal(t, CauseOutcomeAmbiguous, RefusalCauseOf(err), "a relation the build never proved bears the source's name")
}
