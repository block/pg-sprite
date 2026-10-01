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
	assert.Equal(t, want, Insert("app", "orders", "_pgsprite_orders_new", "id", []string{"id", "select", "qty"}))
}
