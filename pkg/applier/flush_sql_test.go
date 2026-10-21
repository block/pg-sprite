package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

const (
	testRelation = `"app"."orders_shadow"`
	testPK       = `"id"`
)

func cols(names ...string) []decode.Column {
	out := make([]decode.Column, len(names))
	for i, n := range names {
		out[i] = decode.PresentColumn(n, n+"-value")
	}
	return out
}

// The upsert writes the key and exactly the present columns, and on conflict
// assigns exactly those columns from EXCLUDED, so a column the image did not
// carry keeps the shadow's value (CO-8).
func TestUpsertSQLAssignsOnlyThePresentColumns(t *testing.T) {
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty", "paid at") VALUES ($1, $2, $3) ON CONFLICT ("id") DO UPDATE SET "qty" = EXCLUDED."qty", "paid at" = EXCLUDED."paid at"`,
		upsertSQL(testRelation, testPK, cols("qty", "paid at"), nil))
}

// An image with no column but the key still has to land the key: the
// conflict clause assigns the key to itself, which PostgreSQL accepts.
func TestUpsertSQLWithOnlyTheKey(t *testing.T) {
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id") VALUES ($1) ON CONFLICT ("id") DO UPDATE SET "id" = EXCLUDED."id"`,
		upsertSQL(testRelation, testPK, nil, nil))
}

// The update for a marker-bearing image assigns the present columns by
// position after the key, so a row count of zero means the row is absent.
func TestUpdateSQLAssignsThePresentColumnsByPosition(t *testing.T) {
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "qty" = $2, "note" = $3 WHERE "id" = $1`,
		updateSQL(testRelation, testPK, cols("qty", "note"), nil))
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "id" = $1 WHERE "id" = $1`,
		updateSQL(testRelation, testPK, nil, nil), "an image of nothing but markers still probes the row")
}

// The fallback's insert is a whole row with no conflict clause: every key
// in the batch was deleted first, so a conflict there is a real collision.
func TestInsertSQLIsAWholeRowWithoutAConflictClause(t *testing.T) {
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty", "note") VALUES ($1, $2, $3)`,
		insertSQL(testRelation, testPK, cols("qty", "note"), nil))
}

// A column the gated statement retyped with USING is written through that
// expression: the parameter is bound as the source's type, under the
// column's own name — the one name the expression may refer to — and the
// expression's result is what the shadow column takes. Every other column
// is still a bare parameter, in the same position.
func TestWritesConvertAColumnThroughItsExpression(t *testing.T) {
	conv := conversions{"qty": {expr: "(qty::numeric * 100)::integer", sourceType: "text"}}
	converted := `(SELECT (qty::numeric * 100)::integer FROM (SELECT $2::text AS "qty") AS "source")`
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty", "note") VALUES ($1, `+converted+`, $3) ON CONFLICT ("id") DO UPDATE SET "qty" = EXCLUDED."qty", "note" = EXCLUDED."note"`,
		upsertSQL(testRelation, testPK, cols("qty", "note"), conv))
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "qty" = `+converted+`, "note" = $3 WHERE "id" = $1`,
		updateSQL(testRelation, testPK, cols("qty", "note"), conv))
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty", "note") VALUES ($1, `+converted+`, $3)`,
		insertSQL(testRelation, testPK, cols("qty", "note"), conv))
}

// A converted column whose value was read back from the shadow is already
// the converted value, so the write binds it bare: converting it again
// would apply the expression to its own output.
func TestWritesBindAShadowFormValueBare(t *testing.T) {
	conv := conversions{"qty": {expr: "(qty::numeric * 100)::integer", sourceType: "text"}}
	completed := []decode.Column{decode.PresentColumn("qty", shadowForm{value: "250"})}
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty") VALUES ($1, $2)`,
		insertSQL(testRelation, testPK, completed, conv))
	bound, err := args(7, completed)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), "250"}, bound, "the bind is the wrapped value")

	null := []decode.Column{decode.PresentColumn("qty", shadowForm{})}
	assert.Equal(t, `INSERT INTO "app"."orders_shadow" ("id", "qty") VALUES ($1, $2)`, insertSQL(testRelation, testPK, null, conv))
	bound, err = args(7, null)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), nil}, bound, "a shadow NULL stays NULL and is not converted")
}

// A source column that collates differently from its type's default is
// bound under that collation, so an expression that compares or orders the
// value sees what it saw over the source column; a column with its type's
// default collation is bound as the type alone.
func TestWritesBindAConvertedValueUnderTheSourceColumnsCollation(t *testing.T) {
	conv := conversions{"w": {expr: "CASE WHEN w < 'm' THEN 1 ELSE 2 END", sourceType: "text", collation: `pg_catalog."C"`}}
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "w" = (SELECT CASE WHEN w < 'm' THEN 1 ELSE 2 END FROM (SELECT $2::text COLLATE pg_catalog."C" AS "w") AS "source") WHERE "id" = $1`,
		updateSQL(testRelation, testPK, cols("w"), conv))
}

// A value in the shadow's form is bound bare whether or not the statement
// converts its column: the form marks where the value was read, and a
// column with no conversion is never converted.
func TestWritesBindAnUnconvertedShadowFormValueBare(t *testing.T) {
	conv := conversions{"qty": {expr: "(qty::numeric * 100)::integer", sourceType: "text"}}
	completed := []decode.Column{decode.PresentColumn("note", shadowForm{value: "as read"})}
	assert.Equal(t, `UPDATE "app"."orders_shadow" SET "note" = $2 WHERE "id" = $1`, updateSQL(testRelation, testPK, completed, conv))
	bound, err := args(7, completed)
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), "as read"}, bound)
}

func TestDeleteSQL(t *testing.T) {
	assert.Equal(t, `DELETE FROM "app"."orders_shadow" WHERE "id" = $1`, deleteSQL(testRelation, testPK))
	assert.Equal(t, `DELETE FROM "app"."orders_shadow" WHERE "id" = ANY($1::bigint[])`, deleteAllSQL(testRelation, testPK))
}

// A completion read renders each marker column as text so the value makes
// the round trip through a text bind whatever its type, and reads the WAL
// insert position last, in the same statement as the values.
func TestRowSQLRendersEachColumnAsTextWithTheReadPosition(t *testing.T) {
	assert.Equal(t,
		`SELECT "blob"::text, "paid at"::text, pg_catalog.pg_current_wal_insert_lsn()::text FROM "app"."orders_shadow" WHERE "id" = $1`,
		rowSQL(testRelation, testPK, []string{"blob", "paid at"}))
}

// args binds the key first and then each column's value in order, which is
// the position every builder assigns; a NULL binds as nil.
func TestArgsBindTheKeyThenTheValues(t *testing.T) {
	bound, err := args(7, []decode.Column{decode.PresentColumn("qty", "qty-value"), decode.PresentColumn("note", nil)})
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), "qty-value", nil}, bound)
}

// A column with no value cannot be bound: a statement that bound it would
// write a value pgoutput never carried over the shadow's (CO-8). The
// refusal names the key and the column.
func TestArgsRefuseAColumnWithNoValue(t *testing.T) {
	bound, err := args(7, []decode.Column{decode.PresentColumn("qty", "2"), decode.AbsentColumn("blob")})
	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "key 7")
	assert.ErrorContains(t, err, "column blob")
	assert.Nil(t, bound)
}

// split keeps the shadow's writable columns in shadow order whatever order
// the image names them in, drops a column the shadow does not hold and the
// key, and reports as a marker both a decoded marker and a column the image
// never named.
func TestSplitFollowsTheShadowColumns(t *testing.T) {
	f := &Flusher{columns: []string{"qty", "blob", "note"}}
	e := &Entry{Key: 1, Kind: Image, Columns: []decode.Column{
		decode.PresentColumn("note", "n"),
		decode.PresentColumn("id", "1"),
		decode.PresentColumn("dropped", "x"),
		decode.PresentColumn("qty", "2"),
		decode.AbsentColumn("blob"),
	}}
	present, markers := f.split(e)
	assert.Equal(t, []decode.Column{decode.PresentColumn("qty", "2"), decode.PresentColumn("note", "n")}, present)
	assert.Equal(t, []string{"blob"}, markers)

	_, markers = f.split(&Entry{Key: 2, Kind: Image, Columns: cols("qty")})
	assert.Equal(t, []string{"blob", "note"}, markers, "a column the image never named is a marker too")
}
