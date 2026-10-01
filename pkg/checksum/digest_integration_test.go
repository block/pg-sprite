package checksum

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// proofFixture is a throwaway schema on a superuser pool, enough to mint
// the copy-and-swap proof the verifier's constructor demands. The
// superuser is a SET-usable member of every role, so no provisioning is
// needed.
type proofFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newProofFixture(t *testing.T) proofFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return proofFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f proofFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// prove mints the copy-and-swap proof for table.
func (f proofFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
	require.NoError(t, err)
	return target
}

// fakeShadow is a Shadow minted by hand, so the constructor's proof checks
// can be exercised against every way a shadow can fail to describe the
// proven target. Passes over real tables use the shadow builder's proof.
type fakeShadow struct {
	schema, source, shadow string
	sourceOID, shadowOID   uint32
	columns                []string
}

func (s fakeShadow) Schema() string        { return s.schema }
func (s fakeShadow) SourceTable() string   { return s.source }
func (s fakeShadow) ShadowTable() string   { return s.shadow }
func (s fakeShadow) SourceOID() uint32     { return s.sourceOID }
func (s fakeShadow) ShadowOID() uint32     { return s.shadowOID }
func (s fakeShadow) CopyColumns() []string { return s.columns }

// The digest statement is the D7 contract in one string: every copy column
// cast to the shadow's type inside a record, hashed per row, aggregated in
// key order, over a closed bigint-typed key range, with every function
// pg_catalog-qualified. Only the table differs between the two sides.
// Quoting goes through pgx.Identifier, so a column named like a keyword
// survives, while the type spelling is format_type's and is not quoted.
func TestDigestSQLIsFrozen(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			"select" text,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	types := []columnType{
		{name: "id", typeName: "bigint"},
		{name: "select", typeName: "character varying(20)"},
		{name: "qty", typeName: "numeric(10,2)"},
	}
	want := `SELECT pg_catalog.count(*),` +
		` pg_catalog.md5(COALESCE(pg_catalog.string_agg(pg_catalog.md5(ROW("id"::bigint, "select"::character varying(20), "qty"::numeric(10,2))::text), '' ORDER BY "id"), ''))` +
		` FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint`
	assert.Equal(t, want, digestSQL(target, f.schema, "_pgsprite_orders_new", types))
	assert.Equal(t, strings.Replace(want, `"_pgsprite_orders_new"`, `"orders"`, 1), digestSQL(target, f.schema, "orders", types),
		"the source side is the same statement over the source table")
}

// Every way a shadow proof can fail to describe the proven target is
// refused before a connection is opened (ST-6); a good shadow still needs
// a lock session (LK-1).
func TestNewVerifierRefusesAShadowThatIsNotTheTargets(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	good := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "qty"},
	}
	cases := map[string]struct {
		shadow copier.Shadow
		detail string
	}{
		"nil shadow":                       {nil, "verification requires a built shadow"},
		"zero shadow":                      {fakeShadow{}, "shadow proof is empty"},
		"other schema":                     {withSchema(good, "other"), "shadow is for other.orders, proof is for " + f.schema + ".orders"},
		"other source":                     {withSource(good, "invoices"), "shadow is for " + f.schema + ".invoices, proof is for " + f.schema + ".orders"},
		"shadow is the source by name":     {withShadowTable(good, "orders"), "shadow of " + f.schema + ".orders is the source table itself"},
		"no source OID":                    {withOIDs(good, 0, 2), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"no shadow OID":                    {withOIDs(good, 1, 0), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"shadow is the source by relation": {withOIDs(good, 7, 7), "shadow proof for " + f.schema + ".orders names relation 7 as both source and shadow"},
		"no columns":                       {withColumns(good), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
		"no primary key":                   {withColumns(good, "qty"), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewVerifier(target, tc.shadow, nil, Options{})
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.EqualError(t, err, "invariant violation (ST-6): "+tc.detail)
		})
	}

	_, err := NewVerifier(target, good, nil, Options{})
	require.ErrorIs(t, err, ErrInvariantViolation, "a good shadow still needs a lock session")
	assert.EqualError(t, err, "invariant violation (LK-1): verification requires a table lock session")
}

func withSchema(s fakeShadow, schema string) fakeShadow { s.schema = schema; return s }
func withSource(s fakeShadow, source string) fakeShadow { s.source = source; return s }
func withShadowTable(s fakeShadow, shadow string) fakeShadow {
	s.shadow = shadow
	return s
}
func withOIDs(s fakeShadow, source, shadow uint32) fakeShadow {
	s.sourceOID, s.shadowOID = source, shadow
	return s
}
func withColumns(s fakeShadow, columns ...string) fakeShadow { s.columns = columns; return s }
