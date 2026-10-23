package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// plantTruncatingGenerateSeries creates, in the fixture schema, a
// generate_series(integer, smallint) that stops one short of its upper bound.
// The index read calls generate_series(1, i.indnkeyatts) with a literal and a
// smallint, so the catalog's (integer, integer) needs a cast on the second
// argument while this overload matches exactly. The cutover gate reads the
// shadow's indexes under LocalSearchPath(<source schema>, "public"):
// pg_catalog is searched implicitly first, but function resolution ranks
// candidates across the whole path, so the exact match beats the catalog's.
// No other argument shape is affected.
func (f shadowFixture) plantTruncatingGenerateSeries(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE FUNCTION %s.generate_series(integer, smallint) RETURNS SETOF integer LANGUAGE sql
		AS 'SELECT pg_catalog.generate_series($1, $2::pg_catalog.int4 OPERATOR(pg_catalog.-) 1)'`)
}

// requirePlantAnswersTheGatePath proves the plant is live where the gate
// reads: on a transaction whose search_path is the one the gate sets, an
// unqualified generate_series(1, 3::smallint) yields two rows, while the
// pg_catalog-qualified call yields the three the catalog's does.
func (f shadowFixture) requirePlantAnswersTheGatePath(t *testing.T) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, tx.Rollback(t.Context())) }()
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(f.schema, "public"))
	require.NoError(t, err)

	var planted, catalog int
	require.NoError(t, tx.QueryRow(t.Context(),
		`SELECT count(*) FROM generate_series(1, 3::smallint)`).Scan(&planted))
	require.NoError(t, tx.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_catalog.generate_series(1, 3::smallint)`).Scan(&catalog))
	require.Equal(t, 2, planted, "the planted overload must answer an unqualified call on the gate's search_path")
	require.Equal(t, 3, catalog)
}

// The gate pairs the source's indexes with the shadow's by definition, and
// reads each index's key columns through generate_series(1, indnkeyatts)
// over a smallint. A generate_series(integer, smallint) planted in the source
// schema that drops the last position makes an unqualified read list no key
// column for the shadow's single-column indexes, so none of them equals its
// source partner: the gate reports every source index unpaired and cutover
// would leave the shadow's indexes under their shadow-derived names. Qualified,
// the read gets the catalog's answer and every index pairs (D8).
func TestGateCutoverPairsIndexesDespiteAPlantedGenerateSeries(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	f.plantTruncatingGenerateSeries(t)
	f.requirePlantAnswersTheGatePath(t)

	ready, err := f.gate(t, s)

	require.NoError(t, err)
	shadow := s.built.ShadowTable()
	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentIndex, SourceName: "orders_pkey", ShadowName: shadow + "_pkey"},
			{Kind: schemachange.DependentIndex, SourceName: "orders_qty_idx", ShadowName: shadow + "_qty_idx"},
		},
		UnpairedSource: []string{"orders_note_idx"},
		UnpairedShadow: []string{},
	}, ready.Indexes())
}
