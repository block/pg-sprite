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
		upsertSQL(testRelation, testPK, cols("qty", "paid at")))
}

// An image with no column but the key still has to land the key: the
// conflict clause assigns the key to itself, which PostgreSQL accepts.
func TestUpsertSQLWithOnlyTheKey(t *testing.T) {
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id") VALUES ($1) ON CONFLICT ("id") DO UPDATE SET "id" = EXCLUDED."id"`,
		upsertSQL(testRelation, testPK, nil))
}

// The update for a marker-bearing image assigns the present columns by
// position after the key, so a row count of zero means the row is absent.
func TestUpdateSQLAssignsThePresentColumnsByPosition(t *testing.T) {
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "qty" = $2, "note" = $3 WHERE "id" = $1`,
		updateSQL(testRelation, testPK, cols("qty", "note")))
	assert.Equal(t,
		`UPDATE "app"."orders_shadow" SET "id" = $1 WHERE "id" = $1`,
		updateSQL(testRelation, testPK, nil), "an image of nothing but markers still probes the row")
}

// The fallback's insert is a whole row with no conflict clause: every key
// in the batch was deleted first, so a conflict there is a real collision.
func TestInsertSQLIsAWholeRowWithoutAConflictClause(t *testing.T) {
	assert.Equal(t,
		`INSERT INTO "app"."orders_shadow" ("id", "qty", "note") VALUES ($1, $2, $3)`,
		insertSQL(testRelation, testPK, cols("qty", "note")))
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
