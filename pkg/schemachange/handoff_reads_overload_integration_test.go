package schemachange

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// handoffReadFixture is a throwaway schema holding a source table with an
// identity column and a serial column, and a shadow whose id defaults to
// the source's identity sequence, as the build leaves it.
type handoffReadFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newHandoffReadFixture(t *testing.T) handoffReadFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := handoffReadFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, `
		CREATE TABLE %s.src (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			ser serial,
			qty integer
		)`)
	f.exec(t, `CREATE SEQUENCE %s.other_seq`)
	return f
}

// exec runs ddl with every %s replaced by the quoted fixture schema.
func (f handoffReadFixture) exec(t *testing.T, ddl string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(ddl, "%s", pgx.Identifier{f.schema}.Sanitize()))
	require.NoError(t, err)
}

// beginOnSourcePath opens a transaction on the shadow build's path,
// LocalSearchPath(<source schema>, "public"), where an operator planted in
// the source schema is a candidate for every unqualified comparison. The
// handoff reads normally run on a pg_catalog-only path; this pins that
// their answer does not depend on it.
func (f handoffReadFixture) beginOnSourcePath(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
	})
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(f.schema, "public"))
	require.NoError(t, err)
	return tx
}

func (f handoffReadFixture) identity() IdentityColumn {
	return IdentityColumn{Column: "id", SequenceSchema: f.schema, SequenceName: "src_id_seq"}
}

// A "=" planted for exactly (oid, regclass) that is the real equality
// except that it never matches pg_class or a sequence. It outranks the
// catalog's oid = oid, which needs a cast on the right side, so an
// unqualified read finds no pg_depend edge and no sequence row. Each
// qualified read still returns the real catalog's answer.
func TestHandoffReadsIgnoreAHidingEqualityOnTheSourceSchemaPath(t *testing.T) {
	f := newHandoffReadFixture(t)
	f.exec(t, `
		CREATE TABLE %s.shd (
			id  bigint PRIMARY KEY DEFAULT nextval('%s.src_id_seq'::regclass),
			ser integer,
			qty integer
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.equal_unless_class_or_sequence(oid, regclass) RETURNS boolean LANGUAGE sql
		AS 'SELECT $1 OPERATOR(pg_catalog.=) $2::oid
		        AND $2::oid OPERATOR(pg_catalog.<>) ''pg_catalog.pg_class''::pg_catalog.regclass::oid
		        AND NOT EXISTS (
		            SELECT 1
		            FROM pg_catalog.pg_class c
		            WHERE c.oid OPERATOR(pg_catalog.=) $2::oid
		              AND c.relkind OPERATOR(pg_catalog.=) ''S'')'`)
	f.exec(t, `
		CREATE OPERATOR %s.= (
			LEFTARG = oid,
			RIGHTARG = regclass,
			FUNCTION = %s.equal_unless_class_or_sequence
		)`)
	tx := f.beginOnSourcePath(t)
	srcOID := relationOID(t, tx, f.schema, "src")
	shdOID := relationOID(t, tx, f.schema, "shd")

	owned, err := readOwnedSequences(t.Context(), tx, srcOID)
	require.NoError(t, err)
	assert.Equal(t, []OwnedSequence{{Column: "ser", SequenceSchema: f.schema, SequenceName: "src_ser_seq"}}, owned)

	assert.NoError(t, verifyIdentityDefaults(t.Context(), tx, shdOID, []IdentityColumn{f.identity()}))

	oldType, newType, err := readIdentityTypes(t.Context(), tx, sequenceRef{schema: f.schema, name: "src_id_seq"}, f.schema, "src", "id")
	require.NoError(t, err)
	const int8OID = 20 // pg_catalog.int8: the sequence and the column are both bigint
	assert.Equal(t, uint32(int8OID), oldType)
	assert.Equal(t, uint32(int8OID), newType)
}

// The safety side of the same read: a "=" planted for (oid, regclass) that
// matches everything would let an unqualified verifyIdentityDefaults accept
// any default with a dependency at all. Here the shadow's id defaults to a
// different sequence, so handing off src_id_seq would leave the shadow and
// source on separate counters; the qualified read still refuses it.
func TestVerifyIdentityDefaultsRefusesARepointedDefaultDespiteAMatchingEquality(t *testing.T) {
	f := newHandoffReadFixture(t)
	f.exec(t, `
		CREATE TABLE %s.shd (
			id  bigint PRIMARY KEY DEFAULT nextval('%s.other_seq'::regclass),
			ser integer,
			qty integer
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.always_equal(oid, regclass) RETURNS boolean LANGUAGE sql
		AS 'SELECT true'`)
	f.exec(t, `
		CREATE OPERATOR %s.= (
			LEFTARG = oid,
			RIGHTARG = regclass,
			FUNCTION = %s.always_equal
		)`)
	tx := f.beginOnSourcePath(t)
	shdOID := relationOID(t, tx, f.schema, "shd")

	err := verifyIdentityDefaults(t.Context(), tx, shdOID, []IdentityColumn{f.identity()})

	require.Error(t, err)
	assert.Equal(t, CauseIdentityHandoff, RefusalCauseOf(err))
}
