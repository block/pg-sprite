package schemachange

import (
	"slices"
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
// each with the given operator class as its type's default and the
// column's own collation.
func btreeOn(opclass string, columns ...string) indexDefinition {
	opclasses := make([]string, len(columns))
	collations := make([]string, len(columns))
	defaults := make([]bool, len(columns))
	own := make([]bool, len(columns))
	options := make([]int16, len(columns))
	for i := range columns {
		opclasses[i] = opclass
		defaults[i] = true
		own[i] = true
	}
	return indexDefinition{
		AccessMethod:     "btree",
		KeyColumns:       columns,
		Included:         []string{},
		Opclasses:        opclasses,
		Collations:       collations,
		DefaultOpclasses: defaults,
		OwnCollations:    own,
		Options:          options,
	}
}

// withExplicitOpclass gives the first key column an operator class written
// in the index, one that is not its type's default.
func withExplicitOpclass(d indexDefinition, opclass string) indexDefinition {
	d.Opclasses = slices.Clone(d.Opclasses)
	d.DefaultOpclasses = slices.Clone(d.DefaultOpclasses)
	d.Opclasses[0] = opclass
	d.DefaultOpclasses[0] = false
	return d
}

// withOwnCollation gives the first key column its column's own collation.
func withOwnCollation(d indexDefinition, collation string) indexDefinition {
	d.Collations = slices.Clone(d.Collations)
	d.Collations[0] = collation
	return d
}

// withExplicitCollation gives the first key column a collation written in
// the index, one that is not the column's own.
func withExplicitCollation(d indexDefinition, collation string) indexDefinition {
	d.Collations = slices.Clone(d.Collations)
	d.OwnCollations = slices.Clone(d.OwnCollations)
	d.Collations[0] = collation
	d.OwnCollations[0] = false
	return d
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

// The relaxed pass sees only what the exact pass left on both sides: with a
// leftover on each side it runs, and still cannot hand the shadow index the
// exact pass already took to a second source index.
func TestPairIndexesRelaxedPassRunsOnlyOverExactLeftovers(t *testing.T) {
	source := []indexEntry{
		{name: "orders_id_idx", definition: btreeOn("pg_catalog.int4_ops", "id")},
		{name: "orders_id_idx1", definition: btreeOn("pg_catalog.int8_ops", "id")},
	}
	shadow := []indexEntry{
		{name: "_new_id_idx", definition: btreeOn("pg_catalog.int8_ops", "id")},
		{name: "_new_qty_idx", definition: btreeOn("pg_catalog.int4_ops", "qty")},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Equal(t, DependentPairing{
		Pairs:          []DependentPair{{Kind: DependentIndex, SourceName: "orders_id_idx1", ShadowName: "_new_id_idx"}},
		UnpairedSource: []string{"orders_id_idx"},
		UnpairedShadow: []string{"_new_qty_idx"},
	}, got)
}

// An operator class written in the index survives a rebuild for the new
// type, so only the default one is set aside. Here the retype also moved
// the column to another collation, which both shadow indexes took as their
// own, so neither pairs exactly; the written operator class is what keeps
// the two apart, and each pairs with its own shadow copy whatever order
// the names sort in.
func TestPairIndexesKeepsAnExplicitOpclassAcrossARetype(t *testing.T) {
	plain := withOwnCollation(btreeOn("pg_catalog.bpchar_ops", "code"), "pg_catalog.default")
	source := []indexEntry{
		{name: "orders_code_a_idx", definition: withExplicitOpclass(plain, "pg_catalog.bpchar_pattern_ops")},
		{name: "orders_code_b_idx", definition: plain},
	}
	rebuilt := withOwnCollation(btreeOn("pg_catalog.text_ops", "code"), `pg_catalog."C"`)
	shadow := []indexEntry{
		{name: "_new_code_idx", definition: rebuilt},
		{name: "_new_code_idx1", definition: withExplicitOpclass(rebuilt, "pg_catalog.bpchar_pattern_ops")},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"code": true})
	require.NoError(t, err)

	assert.Equal(t, DependentPairing{
		Pairs: []DependentPair{
			{Kind: DependentIndex, SourceName: "orders_code_a_idx", ShadowName: "_new_code_idx1"},
			{Kind: DependentIndex, SourceName: "orders_code_b_idx", ShadowName: "_new_code_idx"},
		},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}, got)
}

// A collation written in the index survives a rebuild the same way, so two
// indexes on the retyped column that differ only in a written collation
// each pair with the shadow index carrying their own collation.
func TestPairIndexesKeepsAnExplicitCollationAcrossARetype(t *testing.T) {
	source := []indexEntry{
		{name: "orders_code_c_idx", definition: withExplicitCollation(btreeOn("pg_catalog.text_ops", "code"), `pg_catalog."C"`)},
		{name: "orders_code_idx", definition: btreeOn("pg_catalog.text_ops", "code")},
	}
	shadow := []indexEntry{
		{name: "_new_code_idx", definition: btreeOn("pg_catalog.bpchar_ops", "code")},
		{name: "_new_code_idx1", definition: withExplicitCollation(btreeOn("pg_catalog.bpchar_ops", "code"), `pg_catalog."C"`)},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"code": true})
	require.NoError(t, err)

	assert.Equal(t, []DependentPair{
		{Kind: DependentIndex, SourceName: "orders_code_c_idx", ShadowName: "_new_code_idx1"},
		{Kind: DependentIndex, SourceName: "orders_code_idx", ShadowName: "_new_code_idx"},
	}, got.Pairs)
}

// Two source indexes that render alike once relaxed are not shown to be
// the same index, so the relaxed pass pairs neither rather than guess
// which shadow index is whose; both sides stay unpaired and visible.
func TestPairIndexesLeavesSourceIndexesThatRelaxAlikeUnpaired(t *testing.T) {
	source := []indexEntry{
		{name: "orders_id_idx", definition: btreeOn("pg_catalog.int4_ops", "id")},
		{name: "orders_id_idx1", definition: btreeOn("pg_catalog.int4_ops", "id")},
	}
	shadow := []indexEntry{
		{name: "_new_id_idx", definition: btreeOn("pg_catalog.int8_ops", "id")},
		{name: "_new_id_idx1", definition: btreeOn("pg_catalog.int8_ops", "id")},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Equal(t, DependentPairing{
		Pairs:          []DependentPair{},
		UnpairedSource: []string{"orders_id_idx", "orders_id_idx1"},
		UnpairedShadow: []string{"_new_id_idx", "_new_id_idx1"},
	}, got)
}

// The same holds when the shadow side is the ambiguous one: a lone source
// index does not take the first of two shadow indexes that relax alike.
func TestPairIndexesLeavesShadowIndexesThatRelaxAlikeUnpaired(t *testing.T) {
	source := []indexEntry{{name: "orders_id_idx", definition: btreeOn("pg_catalog.int4_ops", "id")}}
	shadow := []indexEntry{
		{name: "_new_id_idx", definition: btreeOn("pg_catalog.int8_ops", "id")},
		{name: "_new_id_idx1", definition: btreeOn("pg_catalog.int8_ops", "id")},
	}

	got, err := pairIndexes(source, shadow, map[string]bool{"id": true})
	require.NoError(t, err)

	assert.Equal(t, DependentPairing{
		Pairs:          []DependentPair{},
		UnpairedSource: []string{"orders_id_idx"},
		UnpairedShadow: []string{"_new_id_idx", "_new_id_idx1"},
	}, got)
}

// withOptions sets the per-column indoption flags on a definition.
func withOptions(d indexDefinition, options ...int16) indexDefinition {
	d.Options = options
	return d
}
