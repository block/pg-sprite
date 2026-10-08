package applier_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
)

// fakeShadow is a Shadow the test mints by hand, so the flusher's proof
// checks can be exercised against every way a shadow can fail to describe
// the proven target. Flushes into real tables use the shadow builder's
// proof.
type fakeShadow struct {
	schema, source, shadow string
	sourceOID, shadowOID   uint32
	columns                []string
}

func (s fakeShadow) Schema() string        { return s.schema }
func (s fakeShadow) SourceTable() string   { return s.source }
func (s fakeShadow) ShadowTable() string   { return s.shadow }
func (s fakeShadow) SourceOID() uint32     { return s.sourceOID }
func (s fakeShadow) ShadowOID() uint32     { return s.shadowOID }
func (s fakeShadow) CopyColumns() []string { return s.columns }

// Every way a shadow proof can fail to describe the proven target is
// refused before a connection is opened (ST-6), and a good shadow still
// needs a lock session for the proven table (LK-1).
func TestNewFlusherRefusesAShadowThatIsNotTheTargets(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 1)
	target := f.prove(t, "orders")
	good := fakeShadow{
		schema: f.schema, source: "orders", shadow: "_pgsprite_orders_new",
		sourceOID: 1, shadowOID: 2,
		columns: []string{"id", "qty"},
	}
	cases := map[string]struct {
		shadow copier.Shadow
		detail string
	}{
		"nil shadow":                       {nil, "flush requires a built shadow"},
		"zero shadow":                      {fakeShadow{}, "shadow proof is empty"},
		"other schema":                     {withSchema(good, "other"), "shadow is for other.orders, proof is for " + f.schema + ".orders"},
		"other source":                     {withSource(good, "invoices"), "shadow is for " + f.schema + ".invoices, proof is for " + f.schema + ".orders"},
		"shadow is the source by name":     {withShadowTable(good, "orders"), "shadow of " + f.schema + ".orders is the source table itself"},
		"no source OID":                    {withOIDs(good, 0, 2), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"no shadow OID":                    {withOIDs(good, 1, 0), "shadow proof for " + f.schema + ".orders carries no relation OIDs"},
		"shadow is the source by relation": {withOIDs(good, 7, 7), "shadow proof for " + f.schema + ".orders names relation 7 as both source and shadow"},
		"no columns":                       {withColumns(good), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
		"no primary key":                   {withColumns(good, "qty"), "shadow copy columns for " + f.schema + ".orders do not include the primary key id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := applier.NewFlusher(target, tc.shadow, nil, applier.Options{})
			require.ErrorIs(t, err, applier.ErrInvariantViolation)
			assert.EqualError(t, err, "invariant violation (ST-6): "+tc.detail)
		})
	}

	_, err := applier.NewFlusher(target, good, nil, applier.Options{})
	require.ErrorIs(t, err, applier.ErrInvariantViolation, "a good shadow still needs a lock session")
	assert.EqualError(t, err, "invariant violation (LK-1): flush requires a table lock session")

	f.exec(t, `CREATE TABLE %s.other (id bigint PRIMARY KEY)`)
	_, err = applier.NewFlusher(target, good, f.lock(t, "other"), applier.Options{})
	require.ErrorIs(t, err, applier.ErrInvariantViolation, "a lock for another table")
	assert.EqualError(t, err, "invariant violation (LK-1): table lock is for "+f.schema+".other, proof is for "+f.schema+".orders")
}

func withSchema(s fakeShadow, schema string) fakeShadow { s.schema = schema; return s }
func withSource(s fakeShadow, source string) fakeShadow { s.source = source; return s }
func withShadowTable(s fakeShadow, shadow string) fakeShadow {
	s.shadow = shadow
	return s
}
func withOIDs(s fakeShadow, source, shadow uint32) fakeShadow {
	s.sourceOID, s.shadowOID = source, shadow
	return s
}
func withColumns(s fakeShadow, columns ...string) fakeShadow { s.columns = columns; return s }

// The flush resolves every unqualified operator in the catalog, whatever
// the session's search_path says (CO-9): a schema-local `=` that never
// matches, placed ahead of pg_catalog on the connection's search_path,
// would otherwise turn every key-targeted write into a miss. The flush's
// upsert, update, delete, and completion read all go through `=`.
func TestFlushIgnoresTheSessionSearchPath(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `CREATE FUNCTION %s.never_eq(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.never_eq)`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)
	f.exec(t, `UPDATE %s.seats SET slot = 'Z' WHERE id = 1`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 3`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 1, col("slot", "Z"), marker("doc"))))
	require.NoError(t, buffer.Add(deleteEvent(2, 3)))
	require.NoError(t, buffer.Add(moveEvent(3, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))

	result, err := p.flush.Flush(t.Context(), shadowing, buffer.Drain(everyKeyLanded()))
	require.NoError(t, err)
	require.NoError(t, buffer.Hold(result.Held))
	assert.Equal(t, applier.Result{Images: 1, Deletes: 2, Held: result.Held}, result)
	require.Len(t, result.Held, 1)
	assert.False(t, result.Held[0].FromSource, "the completion read found the old key's row through the catalog's =")
	require.Equal(t, 1, buffer.Release(passed(result.Held)))
	result, err = p.flush.Flush(t.Context(), shadowing, buffer.Drain(everyKeyLanded()))
	require.NoError(t, err)

	assert.Equal(t, applier.Result{Images: 1}, result)
	f.assertConverged(t, p.shadow)
}

// A completion is a value read as text and bound back as text, and a
// float's text follows extra_float_digits: a connection configured at 0
// renders fifteen significant digits, which is not always the stored value.
// The flush pins the setting at its maximum for its own transaction, so a
// completed float is the stored float (CO-8).
func TestFlushCompletesFloatsExactlyWhateverTheConnectionRenders(t *testing.T) {
	f := newFlushFixture(t)
	f.exec(t, `
		CREATE TABLE %s.readings (
			id bigint PRIMARY KEY,
			value float8,
			samples float8[],
			note text
		)`)
	f.exec(t, `ALTER TABLE %s.readings ALTER COLUMN samples SET STORAGE EXTERNAL`)
	// 0.1 + 0.2 is the float8 whose shortest exact rendering needs
	// seventeen significant digits; fifteen round it to 0.3.
	f.exec(t, `INSERT INTO %s.readings (id, value, samples, note) VALUES (1, 0.1::float8 + 0.2::float8, array_fill(0.1::float8 + 0.2::float8, ARRAY[400]), 'n')`)
	p := f.prepare(t, "readings")
	rounding, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	rounding.ConnConfig.RuntimeParams["extra_float_digits"] = "0"
	pool, err := pgxpool.NewWithConfig(t.Context(), rounding)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var rendered string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT (0.1::float8 + 0.2::float8)::text`).Scan(&rendered))
	require.Equal(t, "0.3", rendered, "the connection rounds floats on its own")
	f.exec(t, `UPDATE %s.readings SET id = 10 WHERE id = 1`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 1, 10, col("value", "0.30000000000000004"), marker("samples"), col("note", "n"))))

	result, err := p.flush.Flush(t.Context(), pool, buffer.Drain(everyKeyLanded()))
	require.NoError(t, err)
	require.NoError(t, buffer.Hold(result.Held))
	require.Len(t, result.Held, 1)
	samples := result.Held[0].Completed[0].Value.(string)
	assert.Contains(t, samples, "0.30000000000000004", "the completion carries every digit")
	assert.NotContains(t, samples, "0.3,", "and never the rounded form")
	require.Equal(t, 1, buffer.Release(passed(result.Held)))
	_, err = p.flush.Flush(t.Context(), pool, buffer.Drain(everyKeyLanded()))
	require.NoError(t, err)

	f.assertConverged(t, p.shadow)
}

// An empty batch opens no transaction: the pool hands out no connection.
func TestFlushOfAnEmptyBatchOpensNoTransaction(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 1)
	p := f.prepare(t, "orders")
	before := f.pool.Stat().AcquireCount()

	result, err := p.flush.Flush(t.Context(), f.pool, applier.Batch{})

	require.NoError(t, err)
	assert.Equal(t, applier.Result{}, result)
	assert.Equal(t, before, f.pool.Stat().AcquireCount(), "no connection was acquired")
}
