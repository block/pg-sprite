package checksum

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The keyed rows statement is the rows statement over a list of keys: the
// same per-row hash returned with its key in key order, for the rows
// whose key is in one bigint array, compared with the catalog's own
// equality, and the same USING expression in a retyped column's place on
// the source side. Quoting goes through pgx.Identifier, so a key column
// named like a keyword survives.
func TestKeyedRowsSQLIsFrozen(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			"select" bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	types := []columnType{
		{name: "select", typeName: "bigint"},
		{name: "qty", typeName: "numeric(10,2)"},
	}
	want := `SELECT "select", pg_catalog.sha256(pg_catalog.convert_to(ROW("select"::bigint, "qty"::numeric(10,2))::text, pg_catalog.getdatabaseencoding()))` +
		` FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "select" OPERATOR(pg_catalog.=) ANY($1::bigint[])` +
		` ORDER BY "select"`
	assert.Equal(t, want, keyedRowsSQL(target, f.schema, "_pgsprite_orders_new", types, nil))

	conversions := map[string]string{"qty": "(NULLIF(TRIM (BOTH  FROM qty), ''))::numeric"}
	converted := strings.Replace(want, `"qty"::numeric(10,2)`, `((NULLIF(TRIM (BOTH  FROM qty), ''))::numeric)::numeric(10,2)`, 1)
	assert.Equal(t, strings.Replace(converted, `"_pgsprite_orders_new"`, `"orders"`, 1), keyedRowsSQL(target, f.schema, "orders", types, conversions),
		"a converted column hashes as its USING expression, cast to the shadow's type, on the source side")
}
