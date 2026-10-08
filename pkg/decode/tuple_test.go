package decode

import (
	"testing"

	"github.com/jackc/pglogrepl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerRelation is a two-column table, bigint id as the key and text note,
// as pgoutput describes it.
func ledgerRelation() *relation {
	return &relation{
		id: 16384,
		columns: []pglogrepl.RelationMessageColumn{
			{Flags: keyFlag, Name: "id", DataType: 20},
			{Name: "note", DataType: 25},
		},
		keyIndex: 0,
	}
}

func textColumn(s string) *pglogrepl.TupleDataColumn {
	return &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeText, Data: []byte(s)}
}

func tuple(columns ...*pglogrepl.TupleDataColumn) *pglogrepl.TupleData {
	return &pglogrepl.TupleData{ColumnNum: uint16(len(columns)), Columns: columns}
}

// A text value is carried as a string and NULL as nil; both are present,
// so an applier writes them. Only the unchanged-TOAST marker is absent.
func TestDecodeColumnsDistinguishesNullFromAnUnchangedToastMarker(t *testing.T) {
	rel := ledgerRelation()

	columns, err := decodeColumns(rel, tuple(textColumn("7"), &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeNull}))
	require.NoError(t, err)
	assert.Equal(t, []Column{{Name: "id", Value: "7", Present: true}, {Name: "note", Value: nil, Present: true}}, columns)

	columns, err = decodeColumns(rel, tuple(textColumn("7"), &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeToast}))
	require.NoError(t, err)
	assert.Equal(t, []Column{{Name: "id", Value: "7", Present: true}, {Name: "note", Value: nil, Present: false}}, columns)
}

// The stream asks for text, so binary tuple data is something the server
// cannot have sent for this stream and is refused rather than mis-typed.
func TestDecodeColumnsRefusesBinaryData(t *testing.T) {
	_, err := decodeColumns(ledgerRelation(), tuple(textColumn("7"), &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeBinary, Data: []byte{0}}))
	require.ErrorIs(t, err, ErrInvariantViolation)
}

// A tuple whose column count disagrees with the relation cannot be lined
// up by name; it is refused rather than truncated or padded.
func TestDecodeColumnsRefusesAColumnCountMismatch(t *testing.T) {
	_, err := decodeColumns(ledgerRelation(), tuple(textColumn("7")))
	require.ErrorIs(t, err, ErrInvariantViolation)

	_, err = decodeColumns(ledgerRelation(), nil)
	require.ErrorIs(t, err, ErrInvariantViolation)
}

// The key is read from the key column alone, so a key-only old tuple —
// every other column NULL — yields it; a non-integer key or a key that is
// not text is refused.
func TestDecodeKeyReadsTheKeyColumnOnly(t *testing.T) {
	rel := ledgerRelation()

	key, err := decodeKey(rel, tuple(textColumn("-9223372036854775808"), &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeNull}))
	require.NoError(t, err)
	assert.Equal(t, int64(-9223372036854775808), key)

	_, err = decodeKey(rel, tuple(textColumn("seven"), textColumn("x")))
	require.ErrorIs(t, err, ErrInvariantViolation)

	_, err = decodeKey(rel, tuple(&pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeNull}, textColumn("x")))
	require.ErrorIs(t, err, ErrInvariantViolation)

	_, err = decodeKey(rel, nil)
	require.ErrorIs(t, err, ErrInvariantViolation)
}

// Two descriptions agree only column for column; a changed type modifier
// or key flag is a different shape, as is a different replica identity or
// a different relation under the same name.
func TestRelationSameShapeComparesEveryColumnAttribute(t *testing.T) {
	base := ledgerRelation()
	assert.True(t, base.sameShape(ledgerRelation()))

	retyped := ledgerRelation()
	retyped.columns[1].TypeModifier = 24
	assert.False(t, base.sameShape(retyped))

	reflagged := ledgerRelation()
	reflagged.columns[1].Flags = keyFlag
	assert.False(t, base.sameShape(reflagged))

	widened := ledgerRelation()
	widened.columns = append(widened.columns, pglogrepl.RelationMessageColumn{Name: "flag", DataType: 23})
	assert.False(t, base.sameShape(widened))

	full := ledgerRelation()
	full.replicaIdentity = 'f'
	assert.False(t, base.sameShape(full))

	recreated := ledgerRelation()
	recreated.id++
	assert.False(t, base.sameShape(recreated), "a new OID is a different table")
}
