package schemachange

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// Column ACLs are part of the ST-5 snapshot: a shadow without the source
// grant differs, while applying the same grant makes the snapshots equal.
func TestFidelitySnapshotComparesColumnGrants(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	reader := testutil.NewRole(t, pool, "NOLOGIN")
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+pgx.Identifier{schema, "source"}.Sanitize()+` (id bigint PRIMARY KEY, secret text);
		CREATE TABLE `+pgx.Identifier{schema, "shadow"}.Sanitize()+` (id bigint PRIMARY KEY, secret text);
		GRANT SELECT (secret) ON `+pgx.Identifier{schema, "source"}.Sanitize()+` TO `+pgx.Identifier{reader}.Sanitize())
	require.NoError(t, err)

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(t.Context())) }()
	sourceOID := relationOID(t, tx, schema, "source")
	shadowOID := relationOID(t, tx, schema, "shadow")
	source, err := readFidelity(t.Context(), tx, sourceOID)
	require.NoError(t, err)
	shadow, err := readFidelity(t.Context(), tx, shadowOID)
	require.NoError(t, err)
	assert.NotEqual(t, source, shadow)
	assert.Equal(t, []ColumnGrant{{Column: "secret", Grant: Grant{Privilege: "SELECT", Grantee: reader}}}, source.ColumnGrants)

	_, err = tx.Exec(t.Context(), `GRANT SELECT (secret) ON `+pgx.Identifier{schema, "shadow"}.Sanitize()+` TO `+pgx.Identifier{reader}.Sanitize())
	require.NoError(t, err)
	shadow, err = readFidelity(t.Context(), tx, shadowOID)
	require.NoError(t, err)
	assert.Equal(t, source, shadow)
}

func relationOID(t *testing.T, tx pgx.Tx, schema, table string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT $1::regclass::oid`, pgx.Identifier{schema, table}.Sanitize()).Scan(&oid))
	return oid
}
