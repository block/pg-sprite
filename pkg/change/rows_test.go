package change

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

func ledgerTable() *TableInfo {
	return &TableInfo{SchemaName: "app", TableName: "ledger", Columns: []string{"id", "note"}, KeyColumns: []string{"id"}}
}

func ledgerColumns(id, note string) []decode.Column {
	return []decode.Column{decode.PresentColumn("id", id), decode.PresentColumn("note", note)}
}

// An INSERT is one image under its key, the values as the stream carried
// them, in the table's column order.
func TestRowChangesOfAnInsertIsOneImage(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Insert, Key: 101, Columns: ledgerColumns("101", "new")}

	changes, err := rowChanges(ev, ledgerTable(), false)
	require.NoError(t, err)
	assert.Equal(t, []rowChange{{key: []any{int64(101)}, image: []any{"101", "new"}}}, changes)
}

// A NULL is a value in the image, carried as nil.
func TestRowChangesCarriesANullAsNil(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Insert, Key: 101,
		Columns: []decode.Column{decode.PresentColumn("id", "101"), decode.PresentColumn("note", nil)}}

	changes, err := rowChanges(ev, ledgerTable(), false)
	require.NoError(t, err)
	assert.Equal(t, []rowChange{{key: []any{int64(101)}, image: []any{"101", nil}}}, changes)
}

// An UPDATE that kept its key is one image.
func TestRowChangesOfAnUpdateIsOneImage(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Update, Key: 5, Columns: ledgerColumns("5", "edited")}

	changes, err := rowChanges(ev, ledgerTable(), false)
	require.NoError(t, err)
	assert.Equal(t, []rowChange{{key: []any{int64(5)}, image: []any{"5", "edited"}}}, changes)
}

// An UPDATE that moved the key deletes the old key, then writes the image
// under the new one.
func TestRowChangesOfAKeyMoveIsADeleteThenAnImage(t *testing.T) {
	old := int64(5)
	ev := decode.ChangeEvent{Kind: decode.Update, Key: 5000, OldKey: &old, Columns: ledgerColumns("5000", "row 5")}

	changes, err := rowChanges(ev, ledgerTable(), false)
	require.NoError(t, err)
	assert.Equal(t, []rowChange{
		{key: []any{int64(5)}, deleted: true},
		{key: []any{int64(5000)}, image: []any{"5000", "row 5"}},
	}, changes)
}

// A key move is refused when the subscription declares the key immutable.
func TestRowChangesRefusesAKeyMoveOfAnImmutableKey(t *testing.T) {
	old := int64(5)
	ev := decode.ChangeEvent{Kind: decode.Update, Key: 5000, OldKey: &old, Columns: ledgerColumns("5000", "row 5")}

	_, err := rowChanges(ev, ledgerTable(), true)
	require.ErrorIs(t, err, ErrImmutableColumnChanged)
}

// A DELETE is the key alone.
func TestRowChangesOfADeleteIsTheKey(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Delete, Key: 6}

	changes, err := rowChanges(ev, ledgerTable(), false)
	require.NoError(t, err)
	assert.Equal(t, []rowChange{{key: []any{int64(6)}, deleted: true}}, changes)
}

// An UPDATE that left an out-of-line value unsent has no whole image, so it
// is refused rather than applied with the value missing (CO-8).
func TestRowChangesRefusesAnAbsentColumn(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Update, Key: 7,
		Columns: []decode.Column{decode.PresentColumn("id", "7"), decode.AbsentColumn("note")}}

	_, err := rowChanges(ev, ledgerTable(), false)
	require.ErrorIs(t, err, ErrIncompleteImage)
}

// A change whose columns are not the subscription's, in its order, is an
// invariant violation: the image would land in the wrong columns.
func TestRowChangesRefusesColumnsOutOfTheTablesOrder(t *testing.T) {
	ev := decode.ChangeEvent{Kind: decode.Insert, Key: 101,
		Columns: []decode.Column{decode.PresentColumn("note", "new"), decode.PresentColumn("id", "101")}}

	_, err := rowChanges(ev, ledgerTable(), false)
	require.ErrorIs(t, err, ErrInvariantViolation)
}
