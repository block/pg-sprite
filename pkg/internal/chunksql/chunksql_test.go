package chunksql

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The chunk insert is the CO-4 contract in one string: the shared columns,
// a closed bigint-typed key range, and ON CONFLICT DO NOTHING so the
// statement never overwrites what the applier wrote. Quoting goes through
// pgx.Identifier, so a column named like a keyword survives.
func TestInsertIsFrozen(t *testing.T) {
	want := `INSERT INTO "app"."_pgsprite_orders_new" ("id", "select", "qty")` +
		` SELECT "id", "select", "qty" FROM "app"."orders"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint` +
		` ON CONFLICT ("id") DO NOTHING`
	assert.Equal(t, want, Insert("app", "orders", "_pgsprite_orders_new", "id", []string{"id", "select", "qty"}, nil))
}

// A converted column is selected as its USING expression, aliased to the
// column it lands in; every other column stays bare, and the insert's
// column list is the same whether or not a column is converted. A
// conversion for a column that is not copied is not rendered: the column
// list is the contract, the conversions decorate it.
func TestInsertCarriesConversions(t *testing.T) {
	want := `INSERT INTO "app"."_pgsprite_orders_new" ("id", "select", "qty")` +
		` SELECT "id", length("select") * 2 AS "select", qty::int AS "qty" FROM "app"."orders"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint` +
		` ON CONFLICT ("id") DO NOTHING`
	conversions := map[string]string{
		"select": `length("select") * 2`,
		"qty":    "qty::int",
		"notes":  "notes::text",
	}
	assert.Equal(t, want, Insert("app", "orders", "_pgsprite_orders_new", "id", []string{"id", "select", "qty"}, conversions))
}

// Without conversions the statement names nothing the search_path
// resolves, so the path is the catalog alone; with them the user's
// expressions resolve on the build's path, the source schema then public.
func TestSearchPath(t *testing.T) {
	assert.Equal(t, `SET LOCAL search_path = "pg_catalog"`, SearchPath("app", false))
	assert.Equal(t, `SET LOCAL search_path = "app", "public"`, SearchPath("app", true))
}
