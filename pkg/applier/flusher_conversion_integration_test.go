package applier_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// createPricedOrders creates an orders table whose price is text holding a
// decimal amount, or NULL: the shape a change to an integer cents column
// has to convert. No assignment cast turns text into integer, so a value
// written bare would fail, and the statement's USING is the only way to
// land it; a NULL price converts to zero cents, which a NULL bound bare
// never would.
func (f flushFixture) createPricedOrders(t *testing.T, rows int64) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			price text,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.orders (id, price, note)
		SELECT n, CASE WHEN n %% 10 = 0 THEN NULL ELSE (n || '.25') END, 'order ' || n
		FROM generate_series(1, %d) AS n`, rows))
}

// priceInCents is the conversion every test here retypes price with.
const priceInCents = `(coalesce(price, '0')::numeric * 100)::integer`

// preparePriced proves, locks, shadows, and copies orders with its price
// retyped to integer cents through the given USING expression, and fronts
// the shadow with a flusher.
func (f flushFixture) preparePriced(t *testing.T, using string) prepared {
	t.Helper()
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING `+using)
	f.copy(t, target, shadow, lock)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	return prepared{target: target, lock: lock, shadow: shadow, flush: flush}
}

// assertPricesInCents checks the shadow holds every source row and, for
// each, the source price in integer cents: the value the USING expression
// produces, which no cast of the text alone would.
func (f flushFixture) assertPricesInCents(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	var mismatched int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM `+pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()+` s
		FULL JOIN `+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+` d USING (id)
		WHERE d.price IS DISTINCT FROM (coalesce(s.price, '0')::numeric * 100)::integer`).Scan(&mismatched))
	assert.Zero(t, mismatched, "every shadow row holds its source price in cents")
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"price"}})
}

// A column the change retypes with USING is written through that
// expression on every catch-up path — an inserted row, an updated row,
// and a row updated to NULL — so the shadow holds what the statement would
// have produced, as it does for the rows the copier landed (D7). The
// values are the text pgoutput carries, converted as the source's type.
func TestFlushConvertsARetypedColumnThroughTheStatementsUsing(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 50)
	p := f.preparePriced(t, priceInCents)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	f.exec(t, `UPDATE %s.orders SET price = NULL WHERE id = 6`)
	f.exec(t, `DELETE FROM %s.orders WHERE id = 7`)
	f.exec(t, `INSERT INTO %s.orders (id, price, note) VALUES (51, '0.99', 'order 51')`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
		image(6, col("price", nil), col("note", "order 6")),
		deleted(7),
		image(51, col("price", "0.99"), col("note", "order 51")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 3, Deletes: 1}, result)
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "750", *price5)
	price6, _ := f.text(t, p.shadow.ShadowTable(), "price", 6)
	assert.Equal(t, "0", *price6, "the expression saw the NULL and converted it; the NULL was not bound bare")
	price51, _ := f.text(t, p.shadow.ShadowTable(), "price", 51)
	assert.Equal(t, "99", *price51)
	f.assertPricesInCents(t, p.shadow)
}

// A USING expression resolves its unqualified names the way it did when
// the gated statement ran on the shadow: on the build's search_path, the
// source schema then public, with the catalog still ahead of both (CO-9).
// The expression calls a function that lives only in the source schema,
// which a catalog-only path would not find; and the pool puts the schema
// first, where a `=` that is never true would make every key-targeted
// write a miss if the flush took the connection's path.
func TestFlushResolvesConversionNamesOnTheBuildsSearchPath(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedOrders(t, 20)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(amount text) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT (coalesce(amount, ''0'')::numeric * 100)::integer'`)
	f.exec(t, `CREATE FUNCTION %s.never_eq(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.never_eq)`)
	p := f.preparePriced(t, `to_cents(price)`)
	require.Equal(t, map[string]string{"price": "to_cents(price)"}, p.shadow.Conversions())
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)
	f.exec(t, `UPDATE %s.orders SET price = '7.50' WHERE id = 5`)
	f.exec(t, `DELETE FROM %s.orders WHERE id = 7`)

	result, err := p.flush.Flush(t.Context(), shadowing, batch(
		image(5, col("price", "7.50"), col("note", "order 5")),
		deleted(7),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1, Deletes: 1}, result)
	price5, _ := f.text(t, p.shadow.ShadowTable(), "price", 5)
	assert.Equal(t, "750", *price5, "the schema's function converted the value")
	_, present := f.text(t, p.shadow.ShadowTable(), "price", 7)
	assert.False(t, present, "the delete found its row through the catalog's =")
	f.assertPricesInCents(t, p.shadow)
}

// createPricedSeats creates the CO-6 table with a converted document: a
// unique slot whose values tests exchange, a price in text, and a document
// always stored out of line — so an UPDATE leaving it unchanged decodes it
// as a marker — that the change retypes to bytea through convert_to.
func (f flushFixture) createPricedSeats(t *testing.T, rows int64) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot text UNIQUE,
			price text,
			doc text,
			note text
		)`)
	f.exec(t, `ALTER TABLE %s.seats ALTER COLUMN doc SET STORAGE EXTERNAL`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.seats (id, slot, price, doc, note)
		SELECT n, chr(64 + n::integer), (n || '.25'), repeat(encode(sha256(convert_to(n::text, 'UTF8')), 'hex'), 150), 'seat ' || n
		FROM generate_series(1, %d) AS n`, rows))
}

// preparePricedSeats proves, locks, shadows, and copies seats with its
// price retyped to integer cents and its document to bytea, each through
// USING, and fronts the shadow with a flusher.
func (f flushFixture) preparePricedSeats(t *testing.T) prepared {
	t.Helper()
	target := f.prove(t, "seats")
	lock := f.lock(t, "seats")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN price TYPE integer USING `+priceInCents+`,
			ALTER COLUMN doc TYPE bytea USING convert_to(doc, 'UTF8')`)
	f.copy(t, target, shadow, lock)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	return prepared{target: target, lock: lock, shadow: shadow, flush: flush}
}

// assertSeatsConverted checks every shadow seat holds its source price in
// cents and its source document as the bytes convert_to produces.
func (f flushFixture) assertSeatsConverted(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	var mismatched int64
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM `+pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()+` s
		FULL JOIN `+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+` d USING (id)
		WHERE d.price IS DISTINCT FROM (coalesce(s.price, '0')::numeric * 100)::integer
		   OR d.doc IS DISTINCT FROM convert_to(s.doc, 'UTF8')`).Scan(&mismatched))
	assert.Zero(t, mismatched, "every shadow seat holds its converted price and document")
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"price", "doc"}})
}

// The CO-6 fallback completes a marker from the shadow row under the
// image's own key, and the shadow holds the converted document, not the
// source's text. The whole-row insert writes that completion bare: putting
// it through convert_to again would convert the bytes' text rendering, not
// the document (D13, D7).
func TestFlushFallbackInsertsAShadowCompletionOfAConvertedColumnBare(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedSeats(t, 2)
	p := f.preparePricedSeats(t)
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 1`)
	f.exec(t, `UPDATE %s.seats SET slot = 'A' WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("price", "1.25"), marker("doc")),
		image(2, col("slot", "A"), col("price", "2.25"), marker("doc")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Completed: 2, Fallback: true}, result)
	f.assertSeatsConverted(t, p.shadow)
}

// A move completed from the old key's shadow row carries the converted
// document in its completion, and the flush that later writes the held
// image binds it bare, so the document the marker stood for lands as the
// shadow held it (CO-8, D13).
func TestFlushHoldsAConvertedCompletionInTheShadowsForm(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedSeats(t, 3)
	p := f.preparePricedSeats(t)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), col("price", "2.25"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.False(t, result.Held[0].FromSource, "completed from the old key's shadow row")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	f.assertSeatsConverted(t, p.shadow)
}

// A move completed from the live source row carries the source's text for
// the converted document, and the write converts it like any value from
// the stream: the completion's origin, not the column, decides whether the
// expression runs (D13, D7).
func TestFlushConvertsACompletionReadFromTheSource(t *testing.T) {
	f := newFlushFixture(t)
	f.createPricedSeats(t, 3)
	p := f.preparePricedSeats(t)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), col("price", "2.25"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource)
	doc2, _ := f.text(t, "seats", "doc", 10)
	assert.Equal(t, []decode.Column{col("doc", *doc2)}, result.Held[0].Completed, "the source's text, not yet converted")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	f.assertSeatsConverted(t, p.shadow)
}
