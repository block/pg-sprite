package chunksql

import (
	"strings"
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

// The skipping insert reads the chunk's rows once, inserts them with no
// conflict target so a unique or exclusion collision skips the row instead
// of failing the statement, and reports the keys of the rows it read that
// landed neither now nor earlier. The select list is Insert's: a converted
// column is read as its expression, so a held-off row lands later exactly
// as Insert would have landed it.
func TestInsertSkippingIsFrozen(t *testing.T) {
	want := `WITH src AS MATERIALIZED (SELECT "id", "select", qty::int AS "qty" FROM "app"."orders"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint),` +
		` ins AS (INSERT INTO "app"."_pgsprite_orders_new" ("id", "select", "qty")` +
		` SELECT "id", "select", "qty" FROM src ON CONFLICT DO NOTHING RETURNING "id")` +
		` SELECT (SELECT count(*) FROM ins),` +
		` ARRAY(SELECT src."id"::bigint FROM src` +
		` WHERE NOT EXISTS (SELECT 1 FROM ins WHERE ins."id" = src."id")` +
		` AND NOT EXISTS (SELECT 1 FROM "app"."_pgsprite_orders_new" sh WHERE sh."id" = src."id")` +
		` ORDER BY src."id")`
	conversions := map[string]string{"qty": "qty::int"}
	assert.Equal(t, want, InsertSkipping("app", "orders", "_pgsprite_orders_new", "id", []string{"id", "select", "qty"}, conversions))
}

// The keyed insert is the skipping insert over an explicit key list: the
// one place the two differ is the WHERE clause, so a held-off row is
// selected, inserted, and reported the same way in both.
func TestInsertKeysDiffersFromInsertSkippingOnlyInItsRange(t *testing.T) {
	columns := []string{"id", "select", "qty"}
	skipping := InsertSkipping("app", "orders", "_pgsprite_orders_new", "id", columns, nil)
	keyed := InsertKeys("app", "orders", "_pgsprite_orders_new", "id", columns, nil)
	assert.Contains(t, skipping, ` WHERE "id" BETWEEN $1::bigint AND $2::bigint)`)
	assert.Contains(t, keyed, ` WHERE "id" = ANY($1::bigint[]))`)
	replaced := strings.Replace(skipping, ` WHERE "id" BETWEEN $1::bigint AND $2::bigint)`, ` WHERE "id" = ANY($1::bigint[]))`, 1)
	assert.Equal(t, replaced, keyed)
}

// Without conversions the statement names nothing the search_path
// resolves, so the path is the catalog alone; with them the user's
// expressions resolve on the build's path, the source schema then public.
func TestSearchPath(t *testing.T) {
	assert.Equal(t, `SET LOCAL search_path = "pg_catalog"`, SearchPath("app", false))
	assert.Equal(t, `SET LOCAL search_path = "app", "public"`, SearchPath("app", true))
}
