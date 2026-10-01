package checksum

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The repair's clearing statement is one string: delete the shadow's rows
// over the same closed bigint-typed key range the copy statement inserts
// over, so the recopy that follows puts back exactly the keys this removed.
// Quoting goes through pgx.Identifier, so a key column named like a keyword
// survives.
func TestRepairSQLIsFrozen(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			"select" bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	shadow := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"select", "qty"},
	}
	want := `DELETE FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "select" BETWEEN $1::bigint AND $2::bigint`
	assert.Equal(t, want, repairSQL(target, shadow))
}
