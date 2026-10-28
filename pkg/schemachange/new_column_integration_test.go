package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// Adding a NOT NULL column with no default beside a retype leaves the copy
// nothing to put in it: the column is not in the copy list, so every copied
// row would take its default, and it has none. The server accepts the
// statement on the empty shadow; the build refuses it inside its own
// transaction rather than committing a shadow whose first chunk would fail.
func TestBuildShadowRefusesANewNotNullColumnWithNoDefault(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)

	_, err := f.build(t, "orders", `
		ALTER TABLE %s.orders
			ADD COLUMN region text NOT NULL,
			ALTER COLUMN qty TYPE bigint`)

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseStatementNewColumn, schemachange.RefusalCauseOf(err))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "a refused build creates nothing")
}

// A new column of a domain that refuses NULL, with no default on the column
// or the domain, is refused for the same reason: the column is declared
// nullable, but the server enforces the domain's NOT NULL on every row the
// copy writes.
func TestBuildShadowRefusesANewColumnWhoseDomainRefusesNull(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	f.exec(t, `
		CREATE DOMAIN %s.nonnull_int AS integer
			NOT NULL`)

	_, err := f.build(t, "orders", `
		ALTER TABLE %s.orders
			ADD COLUMN region_code %s.nonnull_int`)

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseStatementNewColumn, schemachange.RefusalCauseOf(err))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "a refused build creates nothing")
}

// Dropping the primary key and declaring a new one under another name
// leaves the shadow without the column every chunk range, watermark, and
// streamed change is keyed on. The new key's own declaration does not lift
// the refusal: the copy is driven by the source's key, which the shadow no
// longer has.
func TestBuildShadowRefusesAStatementThatReplacesThePrimaryKey(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)

	_, err := f.build(t, "orders", `
		ALTER TABLE %s.orders
			DROP COLUMN id,
			ADD COLUMN new_id bigint PRIMARY KEY,
			ALTER COLUMN qty TYPE bigint`)

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseStatementNewColumn, schemachange.RefusalCauseOf(err))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "a refused build creates nothing")
}

// A new column the server fills — nullable, or NOT NULL with a default —
// builds beside a retype, and stays out of the copy list: the copy writes
// the kept columns and the server fills the new one on every copied row.
func TestBuildShadowAdmitsANewColumnTheServerFills(t *testing.T) {
	t.Run("nullable", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)

		built, err := f.build(t, "orders", `
			ALTER TABLE %s.orders
				ADD COLUMN region text,
				ALTER COLUMN qty TYPE bigint`)
		require.NoError(t, err)

		assert.Equal(t, []string{"id", "qty", "note", "sku"}, built.CopyColumns())
	})

	t.Run("not null with a default", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)

		built, err := f.build(t, "orders", `
			ALTER TABLE %s.orders
				ADD COLUMN priority integer NOT NULL DEFAULT 0,
				ALTER COLUMN qty TYPE bigint`)
		require.NoError(t, err)

		assert.Equal(t, []string{"id", "qty", "note", "sku"}, built.CopyColumns())
	})
}

// Through the Runner the refusal lands before the run has touched anything:
// the shadow build is the first write, and it rolled back, so there is no
// shadow, no write fence on the source, and no checkpoint row to abandon.
// Without the refusal the run would commit the shadow and abandon itself
// at the first chunk's not_null_violation, reporting a data fault for a
// statement no data could have satisfied.
func TestRunnerRefusesAnUnfillableNewColumnBeforeAnyRowOrFence(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)

	_, err := f.run(t, "orders", `
		ALTER TABLE %s.orders
			ADD COLUMN region text NOT NULL,
			ALTER COLUMN qty TYPE bigint`, schemachange.RunOptions{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseStatementNewColumn, schemachange.RefusalCauseOf(err))
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "no shadow was committed")
	assert.False(t, f.hasFence(t, "orders"), "the source was never fenced")
	_, err = f.store.Inspect(t.Context(), f.schema, "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "the refused run writes no row")
	f.assertWritable(t, "orders")
}
