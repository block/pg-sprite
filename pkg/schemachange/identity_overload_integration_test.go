package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// plantHiddenClassEquality creates, in the fixture schema, a "=" operator
// for exactly (oid, regclass) that is the real equality except that it
// never matches pg_class: the catalog-class column of every pg_depend edge
// between two relations compares unequal to 'pg_class'::regclass. The shadow
// build's transaction-local path is LocalSearchPath(<source schema>,
// "public"): pg_catalog is searched implicitly first, but operator
// resolution ranks candidates across the whole path, so an exact
// (oid, regclass) match beats the catalog's oid = oid, which needs a cast on
// the right side.
func (f shadowFixture) plantHiddenClassEquality(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE FUNCTION %s.equal_unless_pg_class(oid, regclass) RETURNS boolean LANGUAGE sql
		AS 'SELECT $1 OPERATOR(pg_catalog.=) $2::oid
		        AND $2::oid OPERATOR(pg_catalog.<>) ''pg_catalog.pg_class''::pg_catalog.regclass::oid'`)
	f.exec(t, `
		CREATE OPERATOR %s.= (
			LEFTARG = oid,
			RIGHTARG = regclass,
			FUNCTION = %s.equal_unless_pg_class
		)`)
}

// requirePlantAnswersTheBuildPath proves the plant is live where the build
// reads: on a transaction whose search_path is the one the build sets, an
// unqualified oid = regclass comparison of pg_class's own oid with
// 'pg_class'::regclass is false, though the catalog's answer is true.
func (f shadowFixture) requirePlantAnswersTheBuildPath(t *testing.T) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, tx.Rollback(t.Context())) }()
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(f.schema, "public"))
	require.NoError(t, err)

	var equal bool
	require.NoError(t, tx.QueryRow(t.Context(),
		`SELECT 'pg_catalog.pg_class'::regclass::oid = 'pg_catalog.pg_class'::regclass`).Scan(&equal))
	require.False(t, equal, "the planted operator must answer an unqualified = on the build's search_path")
}

// The build reads each identity column's sequence through the internal
// pg_depend edge, whose class columns it compares with 'pg_class'::regclass
// (oid = regclass). A "=" planted in the source schema for exactly that pair
// that never matches pg_class hides every edge: an unqualified read finds an
// identity column with no sequence and refuses to build a shadow for a
// table the engine can hand off, so the schema change is blocked by a read
// the source schema's owner controls. Qualified, the build reads the real
// catalog and records the handoff (D5).
func TestBuildShadowReadsIdentitySequencesDespiteAPlantedEquality(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.plantHiddenClassEquality(t)
	f.requirePlantAnswersTheBuildPath(t)

	built, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)

	require.NoError(t, err)
	assert.Equal(t, []schemachange.IdentityColumn{{
		Column:         "id",
		Always:         true,
		SequenceSchema: f.schema,
		SequenceName:   "widgets_id_seq",
		Options: schemachange.SequenceOptions{
			Start:     1,
			Increment: 1,
			Min:       1,
			Max:       9223372036854775807,
			Cache:     1,
			Cycle:     false,
		},
	}}, built.IdentityColumns())
}

// The build renders each identity column's SET DEFAULT nextval(...) through
// the server's format() on the same path, then executes the text it gets
// back. A format(text, text[]) planted in the source schema matches the
// call's argument types exactly and outranks the catalog's
// format(text, VARIADIC "any"), even though pg_catalog is searched first.
// This plant returns a harmless SELECT 1, so an unqualified call leaves the
// shadow column without its default and the build refuses the handoff;
// qualified, the catalog's format renders the statement and the build
// records the handoff.
func TestBuildShadowFormatsIdentityDefaultDespiteAPlantedFormat(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.format(text, text[]) RETURNS text LANGUAGE sql
		AS 'SELECT ''SELECT 1''::pg_catalog.text'`)

	built, err := f.build(t, "widgets", `ALTER TABLE %s.widgets ALTER COLUMN qty TYPE bigint`)

	require.NoError(t, err)
	require.Len(t, built.IdentityColumns(), 1)
	assert.Equal(t, "id", built.IdentityColumns()[0].Column)
	assert.Equal(t, "widgets_id_seq", built.IdentityColumns()[0].SequenceName)
}
