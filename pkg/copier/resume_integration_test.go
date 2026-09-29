package copier

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The clear statement is the resume half of CO-4 in one string: delete the
// lowest $2 shadow rows whose key is at or above $1 — the first key the
// resumed chunker will cut — ordered along the primary key so every batch
// takes a contiguous stretch. Quoting goes through pgx.Identifier, so a key
// column named like a keyword survives.
func TestClearAboveSQLIsFrozen(t *testing.T) {
	f := newChunkerFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			"order" bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	shadow := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"order", "qty"},
	}
	want := `DELETE FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "order" IN (SELECT "order" FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "order" >= $1::bigint ORDER BY "order" LIMIT $2::bigint)`
	assert.Equal(t, want, clearAboveSQL(target, shadow))
}

// The fence is one lock statement on the shadow in SHARE MODE: the weakest
// mode that conflicts with the ROW EXCLUSIVE lock an insert holds until its
// commit completes, and one that still admits the ACCESS SHARE readers.
// A stronger mode would block readers for nothing; a weaker one would not
// wait for the straggler at all.
func TestFenceStragglersSQLIsFrozen(t *testing.T) {
	shadow := fakeShadow{
		schema: "t_1", source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "qty"},
	}
	assert.Equal(t, `LOCK TABLE "t_1"."_pgsprite_orders_new" IN SHARE MODE`, fenceStragglersSQL(shadow))
}
