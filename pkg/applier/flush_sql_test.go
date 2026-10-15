package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/pkg/decode"
)

const (
	testRelation = `"app"."orders_shadow"`
	testPK       = `"id"`
)

func cols(names ...string) []decode.Column {
	out := make([]decode.Column, len(names))
	for i, n := range names {
		out[i] = decode.Column{Name: n, Value: n + "-value", Present: true}
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
	completed := []decode.Column{{Name: "qty", Value: shadowForm{value: "250"}, Present: true}}
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty") VALUES ($1, $2)`,
		insertSQL(testRelation, testPK, completed, conv))
	assert.Equal(t, []any{int64(7), "250"}, args(7, completed), "the bind is the wrapped value")

	null := []decode.Column{{Name: "qty", Value: shadowForm{}, Present: true}}
	assert.Equal(t, `INSERT INTO "app"."orders_shadow" ("id", "qty") VALUES ($1, $2)`, insertSQL(testRelation, testPK, null, conv))
	assert.Equal(t, []any{int64(7), nil}, args(7, null), "a shadow NULL stays NULL and is not converted")
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
// the position every builder assigns.
func TestArgsBindTheKeyThenTheValues(t *testing.T) {
	assert.Equal(t, []any{int64(7), "qty-value", nil},
		args(7, []decode.Column{{Name: "qty", Value: "qty-value", Present: true}, {Name: "note", Value: nil, Present: true}}))
}

// split keeps the shadow's writable columns in shadow order whatever order
// the image names them in, drops a column the shadow does not hold and the
// key, and reports as a marker both a decoded marker and a column the image
// never named.
func TestSplitFollowsTheShadowColumns(t *testing.T) {
	f := &Flusher{columns: []string{"qty", "blob", "note"}}
	e := &Entry{Key: 1, Kind: Image, Columns: []decode.Column{
		{Name: "note", Value: "n", Present: true},
		{Name: "id", Value: "1", Present: true},
		{Name: "dropped", Value: "x", Present: true},
		{Name: "qty", Value: "2", Present: true},
		{Name: "blob", Present: false},
	}}
	present, markers := f.split(e)
	assert.Equal(t, []decode.Column{{Name: "qty", Value: "2", Present: true}, {Name: "note", Value: "n", Present: true}}, present)
	assert.Equal(t, []string{"blob"}, markers)

	_, markers = f.split(&Entry{Key: 2, Kind: Image, Columns: cols("qty")})
	assert.Equal(t, []string{"blob", "note"}, markers, "a column the image never named is a marker too")
}
