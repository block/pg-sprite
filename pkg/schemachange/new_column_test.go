package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// newColumnFixture is a three-column source — id, qty, note — whose shadow
// kept all three and added one more past them: the added column is a new
// column the copy leaves out, the other three are the source's.
type newColumnFixture struct {
	source, shadow []columnPosition
}

func newNewColumnFixture(added string) newColumnFixture {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}, {name: "note", attnum: 3}}
	shadow := append(append([]columnPosition(nil), source...), columnPosition{name: added, attnum: 4})
	return newColumnFixture{source: source, shadow: shadow}
}

// A new column the copy can leave to the server is not refused here:
// nullable, with a default, generated, or an identity of its own. Each is
// a column the copy omits and the server fills. The identity case is
// refused by the build's new-sequence check instead, which runs after this
// one.
func TestRefuseUnfillableNewColumnsAdmitsAColumnTheServerFills(t *testing.T) {
	for name, column := range map[string]schemadiff.Column{
		"nullable":            {Name: "sku", Type: "text"},
		"not null, default":   {Name: "sku", Type: "text", NotNull: true, Default: "'fresh'::text"},
		"generated":           {Name: "sku", Type: "text", NotNull: true, Default: "(qty)::text", Generated: true},
		"identity of its own": {Name: "sku", Type: "bigint", NotNull: true, Identity: schemadiff.IdentityByDefault},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNewColumnFixture("sku")
			shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "note"}, column}}
			assert.NoError(t, refuseUnfillableNewColumns("public", "orders", "id", f.source, f.shadow, shadow, nil))
		})
	}
}

// A new column NOT NULL with no default is refused: the copied rows have no
// value for it and the server would reject the first chunk. The message
// names the column.
func TestRefuseUnfillableNewColumnsRefusesNotNullWithoutADefault(t *testing.T) {
	f := newNewColumnFixture("sku")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "note"}, {Name: "sku", Type: "text", NotNull: true}}}

	err := refuseUnfillableNewColumns("public", "orders", "id", f.source, f.shadow, shadow, nil)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementNewColumn, RefusalCauseOf(err))
	assert.ErrorContains(t, err, "sku", "the message names the column")
}

// A shadow without the source's primary key — dropped outright, with a key
// under a new name in its place — is refused whatever the new key declares:
// the copy is keyed on the source's column and cannot run without it.
func TestRefuseUnfillableNewColumnsRefusesAShadowWithoutThePrimaryKey(t *testing.T) {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}, {name: "note", attnum: 3}}
	shadow := []columnPosition{{name: "qty", attnum: 2}, {name: "note", attnum: 3}, {name: "new_id", attnum: 4}}
	model := schemadiff.Model{Columns: []schemadiff.Column{{Name: "qty"}, {Name: "note"}, {Name: "new_id", Type: "bigint", NotNull: true, Default: "nextval('orders_new_id_seq'::regclass)"}}}

	err := refuseUnfillableNewColumns("public", "orders", "id", source, shadow, model, nil)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementNewColumn, RefusalCauseOf(err))
	assert.ErrorContains(t, err, "id", "the message names the key")
}

// A column the statement kept, retyped, or dropped outright is not a new
// column, however it is declared on the shadow: a kept NOT NULL column with
// no default is the same column, with its values copied.
func TestRefuseUnfillableNewColumnsIgnoresColumnsTheStatementKeptOrDropped(t *testing.T) {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}, {name: "note", attnum: 3}}
	shadow := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}}
	model := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id", NotNull: true}, {Name: "qty", Type: "bigint", NotNull: true}}}

	assert.NoError(t, refuseUnfillableNewColumns("public", "orders", "id", source, shadow, model, nil))
}

// A new NOT NULL column with no default of its own is not refused when its
// domain has a default: a row that leaves the column out takes the
// domain's default, so the copy has a value for it.
func TestRefuseUnfillableNewColumnsAdmitsANotNullColumnWhoseDomainDefaultsIt(t *testing.T) {
	f := newNewColumnFixture("sku")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "note"}, {Name: "sku", Type: "zero_int", NotNull: true}}}
	domains := map[string]domainFill{"sku": {hasDefault: true}}

	assert.NoError(t, refuseUnfillableNewColumns("public", "orders", "id", f.source, f.shadow, shadow, domains))
}

// A new column declared nullable is still refused when its domain refuses
// NULL and supplies no default: the first copied row would carry NULL into
// it.
func TestRefuseUnfillableNewColumnsRefusesAColumnWhoseDomainRefusesNull(t *testing.T) {
	f := newNewColumnFixture("sku")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "note"}, {Name: "sku", Type: "nonnull_int"}}}
	domains := map[string]domainFill{"sku": {notNull: true}}

	err := refuseUnfillableNewColumns("public", "orders", "id", f.source, f.shadow, shadow, domains)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementNewColumn, RefusalCauseOf(err))
}
