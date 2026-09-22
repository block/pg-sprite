package schemachange_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// Dropping a shadow removes exactly the shadow. The source and the sequence
// its identity column draws from are untouched: the shadow's default depended
// on the source's sequence, never the reverse, so the drop needs no CASCADE
// and the source keeps handing out ids afterwards.
func TestDropShadowRemovesOnlyTheShadow(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	lock := f.lock(t, "orders")
	target := f.prove(t, "orders")
	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`), schemachange.Options{})
	require.NoError(t, err)
	shadow := schemachange.ShadowName(f.schema, "orders")
	require.True(t, f.relationExists(t, shadow))

	var before int64
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`INSERT INTO %s.orders (qty) VALUES (1) RETURNING id`, f.schema)).Scan(&before))

	require.NoError(t, schemachange.DropShadow(t.Context(), f.pool, lock, target, schemachange.Options{}))

	assert.False(t, f.relationExists(t, shadow), "the shadow is gone")
	assert.True(t, f.relationExists(t, "orders"), "the source remains")
	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the source shape is untouched")
	var after int64
	require.NoError(t, f.pool.QueryRow(t.Context(), fmt.Sprintf(`INSERT INTO %s.orders (qty) VALUES (2) RETURNING id`, f.schema)).Scan(&after))
	assert.Equal(t, before+1, after, "the source's identity sequence survived the drop")
}

// A drop with nothing to drop is a distinct, non-fatal outcome the caller
// can treat as already done.
func TestDropShadowReportsAMissingShadow(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	err := schemachange.DropShadow(t.Context(), f.pool, f.lock(t, "widgets"), f.prove(t, "widgets"), schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrShadowNotFound)
}

// A table under the shadow's name that the source's owner does not own is
// not this engine's shadow: the drop refuses it and leaves it in place
// rather than destroying someone else's relation.
func TestDropShadowRefusesARelationAnotherRoleOwns(t *testing.T) {
	f, other := newShadowFixtureWithRole(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	shadow := schemachange.ShadowName(f.schema, "widgets")
	f.exec(t, fmt.Sprintf(`CREATE TABLE %%s.%s (id bigint)`, pgx.Identifier{shadow}.Sanitize()))
	f.exec(t, fmt.Sprintf(`ALTER TABLE %%s.%s OWNER TO %s`, pgx.Identifier{shadow}.Sanitize(), pgx.Identifier{other}.Sanitize()))

	err := schemachange.DropShadow(t.Context(), f.pool, f.lock(t, "widgets"), f.prove(t, "widgets"), schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.True(t, f.relationExists(t, shadow), "the refused relation is left in place")
}

// A view under the shadow's name is not a shadow table either, whoever owns
// it: only a plain table is ever dropped.
func TestDropShadowRefusesAViewUnderTheShadowName(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.widgets (
			id bigint PRIMARY KEY
		)`)
	shadow := schemachange.ShadowName(f.schema, "widgets")
	f.exec(t, fmt.Sprintf(`CREATE VIEW %%s.%s AS SELECT id FROM %%s.widgets`, pgx.Identifier{shadow}.Sanitize()))

	err := schemachange.DropShadow(t.Context(), f.pool, f.lock(t, "widgets"), f.prove(t, "widgets"), schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.True(t, f.relationExists(t, shadow), "the refused relation is left in place")
}
