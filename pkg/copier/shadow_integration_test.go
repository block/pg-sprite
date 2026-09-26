package copier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeShadow is a Shadow the test mints by hand, so the copier's proof
// checks can be exercised against every way a shadow can fail to describe
// the proven target. Copies of real tables use the shadow builder's proof.
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

// The copy statement is the CO-4 contract in one string: the shared
// columns, a closed bigint-typed key range, and ON CONFLICT DO NOTHING so
// the copier never overwrites what the applier wrote. Quoting goes through
// pgx.Identifier, so a column named like a keyword survives.
func TestCopySQLIsFrozen(t *testing.T) {
	f := newChunkerFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			"select" text,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	shadow := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "select", "qty"},
	}
	want := `INSERT INTO "` + f.schema + `"."_pgsprite_orders_new" ("id", "select", "qty")` +
		` SELECT "id", "select", "qty" FROM "` + f.schema + `"."orders"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint` +
		` ON CONFLICT ("id") DO NOTHING`
	assert.Equal(t, want, copySQL(target, shadow))
}

// Every way a shadow proof can fail to describe the proven target is
// refused before a connection is opened (ST-6).
func TestNewCopierRefusesAShadowThatIsNotTheTargets(t *testing.T) {
	f := newChunkerFixture(t)
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
		shadow Shadow
		detail string
	}{
		"nil shadow":     {nil, "copy requires a built shadow"},
		"zero shadow":    {fakeShadow{}, "shadow proof is empty"},
		"other schema":   {withSchema(good, "other"), "shadow is for other.orders, proof is for " + f.schema + ".orders"},
		"other source":   {withSource(good, "invoices"), "shadow is for " + f.schema + ".invoices, proof is for " + f.schema + ".orders"},
		"no source OID":  {withOIDs(good, 0, 2), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"no shadow OID":  {withOIDs(good, 1, 0), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"no columns":     {withColumns(good), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
		"no primary key": {withColumns(good, "qty"), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewCopier(target, tc.shadow, nil, Watermark{}, Options{})
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.EqualError(t, err, "invariant violation (ST-6): "+tc.detail)
		})
	}

	_, err := NewCopier(target, good, nil, Watermark{}, Options{})
	require.ErrorIs(t, err, ErrInvariantViolation, "a good shadow still needs a lock session")
	assert.EqualError(t, err, "invariant violation (LK-1): copy requires a table lock session")
}

func withSchema(s fakeShadow, schema string) fakeShadow { s.schema = schema; return s }
func withSource(s fakeShadow, source string) fakeShadow { s.source = source; return s }
func withOIDs(s fakeShadow, source, shadow uint32) fakeShadow {
	s.sourceOID, s.shadowOID = source, shadow
	return s
}
func withColumns(s fakeShadow, columns ...string) fakeShadow { s.columns = columns; return s }
