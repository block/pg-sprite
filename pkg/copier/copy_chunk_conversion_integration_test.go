package copier_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// createPricedOrders creates an orders table whose price is text holding a
// decimal amount, the shape a change to an integer cents column has to
// convert: no assignment cast turns text into integer, so a bare copy of
// the column fails and only the statement's USING expression can land it.
func (f copierFixture) createPricedOrders(t *testing.T, rows int64) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			price text NOT NULL,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.orders (id, price, note)
		SELECT n, (n || '.25'), 'order ' || n FROM generate_series(1, %d) AS n`, rows))
}

// buildWith runs the shadow builder for target under the given ALTER TABLE,
// with %s standing for the qualified table.
func (f copierFixture) buildWith(t *testing.T, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, alter string) schemachange.BuiltShadow {
	t.Helper()
	st, err := statement.ParseOne(fmt.Sprintf(alter, pgx.Identifier{f.schema, target.Table()}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, st, schemachange.Options{})
	require.NoError(t, err)
	return shadow
}

// assertPricesInCents checks every shadow row holds its source price in
// integer cents: the value the USING expression produces, which no cast of
// the text column alone would.
func (f copierFixture) assertPricesInCents(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	var mismatched int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM `+pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()+` s
		JOIN `+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+` d USING (id)
		WHERE d.price <> (s.price::numeric * 100)::integer`).Scan(&mismatched))
	assert.Zero(t, mismatched, "every copied price is the source's amount in cents")
}

// A column the change retypes with USING is copied through that
// expression: text prices become integer cents, a conversion the server's
// assignment cast cannot make — there is none from text to integer — and
// an explicit cast would get wrong, since the cents are the amount times
// one hundred. The copy lands every row, converted as the statement said.
func TestCopierConvertsRetypedColumnsWithTheStatementsUsing(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	f.createPricedOrders(t, rows)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING (price::numeric * 100)::integer`)
	require.Equal(t, map[string]string{"price": "(price::numeric * 100)::int"}, shadow.Conversions(), "the proof carries the USING expression as the deparser renders it")

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	assert.Equal(t, int64(rows), c.Position().RowsInserted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"price"}})
	f.assertPricesInCents(t, shadow)
}

// A USING expression resolves its unqualified names the way it did when
// the gated statement ran on the shadow: on the build's search_path, the
// source schema then public. The expression here calls a function that
// lives only in the source schema, unqualified; under the catalog-only
// path a plain copy runs under, the name would resolve to nothing and
// every chunk would fail.
func TestCopierResolvesConversionNamesOnTheBuildsSearchPath(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	f.createPricedOrders(t, rows)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(amount text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT (amount::numeric * 100)::integer'`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING to_cents(price)`)
	require.Equal(t, map[string]string{"price": "to_cents(price)"}, shadow.Conversions())

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	assert.Equal(t, int64(rows), c.Position().RowsInserted, "every chunk resolved the schema's function")
	f.assertPricesInCents(t, shadow)
}

// The build's search_path still keeps the catalog ahead of the source
// schema, so a converted copy on a pool that puts the schema first resolves
// the key range's BETWEEN to the catalog's operators like a plain copy
// does (CO-9): the schema's bigint <= that is never true answers nothing,
// and every row lands.
func TestCopierWithConversionsIgnoresTheSessionSearchPath(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	f.createPricedOrders(t, rows)
	f.exec(t, `CREATE FUNCTION %s.never_le(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.<= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.never_le)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.buildWith(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING (price::numeric * 100)::integer`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), shadowing))

	assert.Equal(t, int64(rows), c.Position().RowsInserted, "every row landed through the catalog's operators")
	f.assertPricesInCents(t, shadow)
}
