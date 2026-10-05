package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// createAccounts creates a table whose integer key is the column the retype
// tests widen: the primary key and a descending index sit on that key, a
// third index sits on a column the change leaves alone.
func (f shadowFixture) createAccounts(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.accounts (
			id      integer PRIMARY KEY,
			balance integer NOT NULL,
			label   text
		)`)
	f.exec(t, `CREATE INDEX accounts_id_desc_idx ON %s.accounts (id DESC)`)
	f.exec(t, `CREATE INDEX accounts_balance_idx ON %s.accounts (balance)`)
	f.exec(t, `
		INSERT INTO %s.accounts (id, balance, label)
		SELECT g, g * 10, 'acct ' || g
		FROM generate_series(1, 2500) g`)
}

// indexOpclasses reads the operator class of each key column of one index.
func (f shadowFixture) indexOpclasses(t *testing.T, index string) []string {
	t.Helper()
	var opclasses []string
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT ARRAY(SELECT oc.opcname
		             FROM unnest(i.indclass::oid[]) WITH ORDINALITY u(o, k)
		             JOIN pg_opclass oc ON oc.oid = u.o
		             ORDER BY k)
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = ic.relnamespace
		WHERE n.nspname = $1 AND ic.relname = $2`, f.schema, index).Scan(&opclasses))
	return opclasses
}

// Widening the key from integer to bigint makes PostgreSQL re-create the
// shadow's primary key and descending index for the new type, with
// int8_ops where the source has int4_ops; by exact definition neither would
// pair, and the swap would leave the table with a shadow-named primary key.
// The gate pairs them across the retyped column because every other fact
// about each index agrees, and pairs the balance index exactly (ST-5, D8).
func TestGateCutoverPairsIndexesAcrossARetypedColumn(t *testing.T) {
	f := newShadowFixture(t)
	f.createAccounts(t)
	s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN id TYPE bigint`)
	shadow := s.built.ShadowTable()

	// The precondition the relaxed pass exists for: the opclass differs on
	// the retyped column's indexes and nowhere else.
	assert.Equal(t, []string{"int4_ops"}, f.indexOpclasses(t, "accounts_pkey"))
	assert.Equal(t, []string{"int8_ops"}, f.indexOpclasses(t, shadow+"_pkey"))
	assert.Equal(t, []string{"int4_ops"}, f.indexOpclasses(t, "accounts_balance_idx"))
	assert.Equal(t, []string{"int4_ops"}, f.indexOpclasses(t, shadow+"_balance_idx"))

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentIndex, SourceName: "accounts_balance_idx", ShadowName: shadow + "_balance_idx"},
			{Kind: schemachange.DependentIndex, SourceName: "accounts_id_desc_idx", ShadowName: shadow + "_id_idx"},
			{Kind: schemachange.DependentIndex, SourceName: "accounts_pkey", ShadowName: shadow + "_pkey"},
		},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}, ready.Indexes())
}

// Retyping an integer column to text gives its index a collation as well as
// a new operator class; both are the server's own derivations, so both are
// set aside and the index still pairs (D8).
func TestGateCutoverPairsAnIndexAcrossARetypeThatAddsACollation(t *testing.T) {
	f := newShadowFixture(t)
	f.createAccounts(t)
	s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN balance TYPE text`)
	shadow := s.built.ShadowTable()

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	assert.Contains(t, ready.Indexes().Pairs, schemachange.DependentPair{Kind: schemachange.DependentIndex, SourceName: "accounts_balance_idx", ShadowName: shadow + "_balance_idx"})
}

// Two indexes on one column that differ only in a collation written in the
// index stay distinct across a retype: the server keeps the written
// collation on the rebuild and re-derives only the column's own, so each
// source index pairs with the shadow index that carries its collation, and
// neither takes the other's name at the swap (ST-5, D8). The index names
// sort in the opposite order to the shadow's LIKE-derived ones, so a
// pairing by position would cross them.
func TestGateCutoverRetypeKeepsAnExplicitCollationApart(t *testing.T) {
	f := newShadowFixture(t)
	f.createAccounts(t)
	f.exec(t, `CREATE INDEX accounts_label_plain ON %s.accounts (label)`)
	f.exec(t, `CREATE INDEX accounts_label_c ON %s.accounts (label COLLATE "C")`)
	s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN label TYPE char(20)`)
	shadow := s.built.ShadowTable()

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	pairs := ready.Indexes().Pairs
	assert.Contains(t, pairs, schemachange.DependentPair{Kind: schemachange.DependentIndex, SourceName: "accounts_label_plain", ShadowName: shadow + "_label_idx"})
	assert.Contains(t, pairs, schemachange.DependentPair{Kind: schemachange.DependentIndex, SourceName: "accounts_label_c", ShadowName: shadow + "_label_idx1"})
}

// An operator class written in the index survives a rebuild the same way:
// a char column retyped to text under another collation keeps
// bpchar_pattern_ops where it was written (text is binary-coercible to the
// opclass's type) and moves only the plain index from bpchar_ops to
// text_ops, while both rebuilt indexes take the column's new collation as
// their own. Neither pairs exactly; the written operator class keeps them
// apart, so each pairs with its own shadow copy rather than by position
// (ST-5, D8).
func TestGateCutoverRetypeKeepsAnExplicitOpclassApart(t *testing.T) {
	f := newShadowFixture(t)
	f.createAccounts(t)
	f.exec(t, `ALTER TABLE %s.accounts ALTER COLUMN label TYPE char(20)`)
	f.exec(t, `CREATE INDEX accounts_label_plain ON %s.accounts (label)`)
	f.exec(t, `CREATE INDEX accounts_label_pattern ON %s.accounts (label bpchar_pattern_ops)`)
	s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN label TYPE text COLLATE "C"`)
	shadow := s.built.ShadowTable()

	// The precondition: the plain index's opclass moved with the type, the
	// written one stayed.
	assert.Equal(t, []string{"bpchar_ops"}, f.indexOpclasses(t, "accounts_label_plain"))
	assert.Equal(t, []string{"text_ops"}, f.indexOpclasses(t, shadow+"_label_idx"))
	assert.Equal(t, []string{"bpchar_pattern_ops"}, f.indexOpclasses(t, "accounts_label_pattern"))
	assert.Equal(t, []string{"bpchar_pattern_ops"}, f.indexOpclasses(t, shadow+"_label_idx1"))

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	pairs := ready.Indexes().Pairs
	assert.Contains(t, pairs, schemachange.DependentPair{Kind: schemachange.DependentIndex, SourceName: "accounts_label_plain", ShadowName: shadow + "_label_idx"})
	assert.Contains(t, pairs, schemachange.DependentPair{Kind: schemachange.DependentIndex, SourceName: "accounts_label_pattern", ShadowName: shadow + "_label_idx1"})
}

// A retyped column explains an operator-class difference on its own
// indexes only. When the change also drops the column a third index covers,
// that index stays unpaired on the source side as before, and the retyped
// key's indexes still pair.
func TestGateCutoverRetypePassLeavesADroppedColumnsIndexUnpaired(t *testing.T) {
	f := newShadowFixture(t)
	f.createAccounts(t)
	s := f.stage(t, "accounts", `
		ALTER TABLE %s.accounts
			ALTER COLUMN id TYPE bigint,
			DROP COLUMN balance`)
	shadow := s.built.ShadowTable()

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentIndex, SourceName: "accounts_id_desc_idx", ShadowName: shadow + "_id_idx"},
			{Kind: schemachange.DependentIndex, SourceName: "accounts_pkey", ShadowName: shadow + "_pkey"},
		},
		UnpairedSource: []string{"accounts_balance_idx"},
		UnpairedShadow: []string{},
	}, ready.Indexes())
}
