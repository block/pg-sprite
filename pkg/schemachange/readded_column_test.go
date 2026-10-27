package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// readdFixture is a three-column source — id, qty, note — whose shadow
// re-added one column: that column sits past the three the shadow was
// cloned with, so it is a new column, not the source's.
type readdFixture struct {
	source, shadow []columnPosition
}

func newReaddFixture(readded string) readdFixture {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}, {name: "note", attnum: 3}}
	shadow := make([]columnPosition, 0, 3)
	for _, c := range source {
		if c.name != readded {
			shadow = append(shadow, columnPosition{name: c.name, attnum: c.attnum})
		}
	}
	return readdFixture{source: source, shadow: append(shadow, columnPosition{name: readded, attnum: 4})}
}

// A re-added column the copy can leave to its default is not refused:
// nullable, with a default, generated, or an identity of its own replacing a
// plain column. Each is a new column the copy omits and the server fills.
func TestRefuseUnfillableReaddsAdmitsAColumnTheServerFills(t *testing.T) {
	for name, column := range map[string]schemadiff.Column{
		"nullable":            {Name: "note", Type: "text"},
		"not null, default":   {Name: "note", Type: "text", NotNull: true, Default: "'fresh'::text"},
		"generated":           {Name: "note", Type: "text", NotNull: true, Default: "(qty)::text", Generated: true},
		"identity of its own": {Name: "note", Type: "bigint", NotNull: true, Identity: schemadiff.IdentityByDefault},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReaddFixture("note")
			shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, column}}
			assert.NoError(t, refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, nil, nil))
		})
	}
}

// Re-adding the primary key is refused whatever the new column declares:
// the copy is keyed on it and cannot run without it.
func TestRefuseUnfillableReaddsRefusesThePrimaryKey(t *testing.T) {
	f := newReaddFixture("id")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "qty"}, {Name: "note"}, {Name: "id", Type: "bigint", NotNull: true, Default: "nextval('orders_id_seq'::regclass)"}}}

	err := refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, nil, nil)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementReadd, RefusalCauseOf(err))
}

// Re-adding a column NOT NULL with no default is refused: the copied rows
// have no value for it and the server would reject the first chunk.
func TestRefuseUnfillableReaddsRefusesNotNullWithoutADefault(t *testing.T) {
	f := newReaddFixture("qty")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "note"}, {Name: "qty", Type: "integer", NotNull: true}}}

	err := refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, nil, nil)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementReadd, RefusalCauseOf(err))
}

// Replacing a source identity column is refused however the new column is
// declared, even as a nullable plain column the copy could otherwise leave
// to NULL: the source sequence is handed to the same column on the shadow,
// and a new column is not that column.
func TestRefuseUnfillableReaddsRefusesAReplacedIdentityColumn(t *testing.T) {
	f := newReaddFixture("note")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "qty"}, {Name: "note", Type: "text"}}}
	identities := []IdentityColumn{{Column: "note", SequenceSchema: "public", SequenceName: "orders_note_seq"}}

	err := refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, nil, identities)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementReadd, RefusalCauseOf(err))
}

// A column the statement kept, retyped, or dropped outright is not a re-add,
// however it is declared on the shadow: a kept NOT NULL column with no
// default is the same column, with its values copied.
func TestRefuseUnfillableReaddsIgnoresColumnsTheStatementKeptOrDropped(t *testing.T) {
	source := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}, {name: "note", attnum: 3}}
	shadow := []columnPosition{{name: "id", attnum: 1}, {name: "qty", attnum: 2}}
	model := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id", NotNull: true}, {Name: "qty", Type: "bigint", NotNull: true}}}
	identities := []IdentityColumn{{Column: "id"}}

	assert.NoError(t, refuseUnfillableReadds("public", "orders", "id", source, shadow, model, nil, identities))
}

// A re-added NOT NULL column with no default of its own is not refused when
// its domain has a default: a row that leaves the column out takes the
// domain's default, so the copy has a value for it.
func TestRefuseUnfillableReaddsAdmitsANotNullColumnWhoseDomainDefaultsIt(t *testing.T) {
	f := newReaddFixture("qty")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "note"}, {Name: "qty", Type: "zero_int", NotNull: true}}}
	domains := map[string]domainFill{"qty": {hasDefault: true}}

	assert.NoError(t, refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, domains, nil))
}

// A re-added column declared nullable is still refused when its domain
// refuses NULL and supplies no default: the first copied row would carry
// NULL into it.
func TestRefuseUnfillableReaddsRefusesAColumnWhoseDomainRefusesNull(t *testing.T) {
	f := newReaddFixture("qty")
	shadow := schemadiff.Model{Columns: []schemadiff.Column{{Name: "id"}, {Name: "note"}, {Name: "qty", Type: "nonnull_int"}}}
	domains := map[string]domainFill{"qty": {notNull: true}}

	err := refuseUnfillableReadds("public", "orders", "id", f.source, f.shadow, shadow, domains, nil)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Equal(t, CauseStatementReadd, RefusalCauseOf(err))
}
