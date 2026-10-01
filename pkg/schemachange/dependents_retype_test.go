package schemachange

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A column is retyped when the source and the shadow both carry it and
// their canonical types differ; a column the statement dropped or left
// alone is not. Each retyped name is recorded bare and quoted, since
// pg_get_indexdef quotes a key column only when the name needs it.
func TestRetypedColumnsNamesOnlyColumnsWhoseTypeChanged(t *testing.T) {
	source := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "integer"},
		{Name: "Qty", Type: "integer"},
		{Name: "note", Type: "text"},
		{Name: "sku", Type: "text"},
	}}
	shadow := schemadiff.Model{Columns: []schemadiff.Column{
		{Name: "id", Type: "bigint"},
		{Name: "Qty", Type: "numeric(10,2)"},
		{Name: "sku", Type: "text"},
	}}

	got := retypedColumns(source, shadow)

	assert.Equal(t, map[string]bool{
		"id": true, `"id"`: true,
		"Qty": true, `"Qty"`: true,
	}, got)
}

// btreeOn is a plain btree index definition over the given key columns,
// each with the given operator class.
func btreeOn(opclass string, columns ...string) indexDefinition {
	opclasses := make([]string, len(columns))
	collations := make([]string, len(columns))
	options := make([]int16, len(columns))
	for i := range columns {
		opclasses[i] = opclass
	}
	return indexDefinition{
		AccessMethod: "btree",
		KeyColumns:   columns,
		Included:     []string{},
		Opclasses:    opclasses,
		Collations:   collations,
		Options:      options,
	}
}

// primaryKeyOn is the constraint-backed primary-key index over one column.
func primaryKeyOn(opclass, column string) indexDefinition {
	d := btreeOn(opclass, column)
	d.Unique, d.Primary = true, true
	d.Constraint = "PRIMARY KEY (" + column + ")"
	return d
}

// Retyping a key column makes PostgreSQL re-create its indexes with the new
// type's default operator class, so the shadow's primary key and secondary
// index on that column no longer equal the source's exactly; they pair in
// the relaxed pass because everything but the operator class agrees, while
// the index on the untouched column pairs exactly. The pairs come back in
// source order whichever pass made them.
func TestPairIndexesPairsAcrossARetypedColumn(t *testing.T) {
	source := []indexEntry{
		{name: "orders_id_desc_idx", definition: withOptions(btreeOn("pg_catalog.int4_ops", "id"), 1)},
		{name: "orders_pkey", definition: primaryKeyOn("pg_catalog.int4_ops", "id")},
		{name: "orders_qty_idx", definition: btreeOn("pg_catalog.int4_ops", "qty")},
	}
	shadow := []indexEntry{
		{name: "_new_id_desc_idx", definition: withOptions(btreeOn("pg_catalog.int8_ops", "id"), 1)},
		{name: "_new_pkey", definition: primaryKeyOn("pg_catalog.int8_ops", "id")},
		{name: "_new_qty_idx", definition: btreeOn("pg_catalog.int4_ops", "qty")},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Equal(t, DependentPairing{
		Pairs: []DependentPair{
			{Kind: DependentIndex, SourceName: "orders_id_desc_idx", ShadowName: "_new_id_desc_idx"},
			{Kind: DependentIndex, SourceName: "orders_pkey", ShadowName: "_new_pkey"},
			{Kind: DependentIndex, SourceName: "orders_qty_idx", ShadowName: "_new_qty_idx"},
		},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}, got)
}

// Without a retyped column the relaxed pass never runs: the same operator
// class difference leaves both indexes unpaired, because nothing in the
// gated statement explains it.
func TestPairIndexesKeepsAnOpclassDifferenceOnAnUntouchedColumnUnpaired(t *testing.T) {
	source := []indexEntry{{name: "orders_qty_idx", definition: btreeOn("pg_catalog.int4_ops", "qty")}}
	shadow := []indexEntry{{name: "_new_qty_idx", definition: btreeOn("pg_catalog.int8_ops", "qty")}}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Empty(t, got.Pairs)
	assert.Equal(t, []string{"orders_qty_idx"}, got.UnpairedSource)
	assert.Equal(t, []string{"_new_qty_idx"}, got.UnpairedShadow)
}

// The relaxed pass sets aside the operator class and collation only; an
// index on the retyped column whose predicate or ordering also differs is
// a different index and stays unpaired.
func TestPairIndexesRelaxesOnlyOpclassAndCollation(t *testing.T) {
	partial := btreeOn("pg_catalog.int4_ops", "id")
	partial.Predicate = "(id > 100)"
	source := []indexEntry{
		{name: "orders_id_desc_idx", definition: withOptions(btreeOn("pg_catalog.int4_ops", "id"), 1)},
		{name: "orders_id_partial_idx", definition: partial},
	}
	ascending := btreeOn("pg_catalog.int8_ops", "id")
	fullRange := btreeOn("pg_catalog.int8_ops", "id")
	shadow := []indexEntry{
		{name: "_new_id_asc_idx", definition: ascending},
		{name: "_new_id_full_idx", definition: fullRange},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Empty(t, got.Pairs)
	assert.Equal(t, []string{"orders_id_desc_idx", "orders_id_partial_idx"}, got.UnpairedSource)
	assert.Equal(t, []string{"_new_id_asc_idx", "_new_id_full_idx"}, got.UnpairedShadow)
}

// An expression over a retyped column is rendered by the server and may
// read differently for the new type, so it is held to an exact match: the
// operator class is not set aside for it.
func TestPairIndexesDoesNotRelaxAnExpressionOverARetypedColumn(t *testing.T) {
	source := []indexEntry{{name: "orders_id_plus_idx", definition: btreeOn("pg_catalog.int4_ops", "(id + 1)")}}
	shadow := []indexEntry{{name: "_new_id_plus_idx", definition: btreeOn("pg_catalog.int8_ops", "(id + 1)")}}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Empty(t, got.Pairs)
	assert.Equal(t, []string{"orders_id_plus_idx"}, got.UnpairedSource)
	assert.Equal(t, []string{"_new_id_plus_idx"}, got.UnpairedShadow)
}

// A shadow index that paired exactly is spoken for: the relaxed pass works
// only on what the exact pass left on both sides, so a source index on the
// retyped column cannot take a partner that another source index already
// matched exactly.
func TestPairIndexesRelaxedPassUsesOnlyExactLeftovers(t *testing.T) {
	source := []indexEntry{
		{name: "orders_id_idx", definition: btreeOn("pg_catalog.int4_ops", "id")},
		{name: "orders_id_idx1", definition: btreeOn("pg_catalog.int8_ops", "id")},
	}
	shadow := []indexEntry{{name: "_new_id_idx", definition: btreeOn("pg_catalog.int8_ops", "id")}}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Equal(t, []DependentPair{
		{Kind: DependentIndex, SourceName: "orders_id_idx1", ShadowName: "_new_id_idx"},
	}, got.Pairs)
	assert.Equal(t, []string{"orders_id_idx"}, got.UnpairedSource)
	assert.Empty(t, got.UnpairedShadow)
}

// withOptions sets the per-column indoption flags on a definition.
func withOptions(d indexDefinition, options ...int16) indexDefinition {
	d.Options = options
	return d
}
