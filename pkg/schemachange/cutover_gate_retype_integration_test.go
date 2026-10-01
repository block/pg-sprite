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
