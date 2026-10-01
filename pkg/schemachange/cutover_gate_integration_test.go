package schemachange_test

import (
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// gateChunks sizes the copy and the checksum pass to fixed thousand-row
// chunks, so a 2500-row table is three chunks whatever the timing says.
var gateChunks = copier.ChunkerOptions{InitialRows: 1000, MinRows: 1000, MaxRows: 1000}

// staged is one table carried through build, copy, and checksum under one
// lock session: everything GateCutover takes as input.
type staged struct {
	target   preflight.CopySwapTarget
	lock     *dbconn.TableLockSession
	built    schemachange.BuiltShadow
	verified checksum.VerifiedShadow
}

// createOrders creates the orders table the gate tests swap: a serial key
// whose sequence the swap must re-own, two plain indexes, and one extended
// statistics object, holding 2500 rows with distinct qty values.
func (f shadowFixture) createOrders(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		)`)
	f.exec(t, `CREATE INDEX orders_qty_idx ON %s.orders (qty)`)
	f.exec(t, `CREATE INDEX orders_note_idx ON %s.orders (note)`)
	f.exec(t, `CREATE STATISTICS %s.orders_qty_sku_stat ON qty, sku FROM %s.orders`)
	f.exec(t, `
		INSERT INTO %s.orders (qty, note, sku)
		SELECT g, 'note ' || g, 'sku' || (g % 7)
		FROM generate_series(1, 2500) g`)
}

// stage proves, locks, builds the shadow of, copies, and verifies table
// under the given ALTER TABLE, returning everything the gate needs.
func (f shadowFixture) stage(t *testing.T, table, alter string) staged {
	t.Helper()
	return f.stageUnder(t, f.lock(t, table), table, alter)
}

// stageUnder is stage with a lock session the caller owns.
func (f shadowFixture) stageUnder(t *testing.T, lock *dbconn.TableLockSession, table, alter string) staged {
	t.Helper()
	target := f.prove(t, table)
	built, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, alter), schemachange.Options{})
	require.NoError(t, err)
	c, err := copier.NewCopier(target, built, lock, copier.Watermark{}, copier.Options{Workers: 2, Chunker: gateChunks})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	v, err := checksum.NewVerifier(target, built, lock, checksum.Options{Chunker: gateChunks})
	require.NoError(t, err)
	outcome, err := v.Check(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort)
	require.NoError(t, err)
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted, "a clean complete pass mints the verified shadow")
	return staged{target: target, lock: lock, built: built, verified: verified}
}

// gate runs the cutover gate over a staged table.
func (f shadowFixture) gate(t *testing.T, s staged) (schemachange.CutoverReady, error) {
	t.Helper()
	return schemachange.GateCutover(t.Context(), f.pool, s.lock, s.built, s.verified, schemachange.Options{})
}

// shadowName is the schema-qualified, quoted shadow table.
func (f shadowFixture) shadowName(built schemachange.BuiltShadow) string {
	return pgx.Identifier{f.schema, built.ShadowTable()}.Sanitize()
}

// A shadow the builder made, the copier filled, and the verifier proved
// passes the gate, which pairs each source dependent with its shadow
// counterpart by definition, not by name: LIKE named the shadow's primary
// key, qty index, and statistics object after the shadow, and the gate
// still finds them. The index on the dropped note column has no shadow
// partner and is reported, not refused; the serial key's sequence is listed
// for re-owning (ST-5, D5, D8).
func TestGateCutoverPairsDependentsByDefinitionAndListsOwnedSequences(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	shadow := s.built.ShadowTable()
	assert.Equal(t, s.built, ready.Built())
	assert.Equal(t, s.verified, ready.Verified())
	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentIndex, SourceName: "orders_pkey", ShadowName: shadow + "_pkey"},
			{Kind: schemachange.DependentIndex, SourceName: "orders_qty_idx", ShadowName: shadow + "_qty_idx"},
		},
		UnpairedSource: []string{"orders_note_idx"},
		UnpairedShadow: []string{},
	}, ready.Indexes())
	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentStatistics, SourceName: "orders_qty_sku_stat", ShadowName: shadow + "_qty_sku_stat"},
		},
		UnpairedSource: []string{},
		UnpairedShadow: []string{},
	}, ready.Statistics())
	assert.Equal(t, []schemachange.OwnedSequence{
		{Column: "id", SequenceSchema: f.schema, SequenceName: "orders_id_seq"},
	}, ready.OwnedSequences())
}

// A unique constraint the gated statement adds gives the shadow an index
// no source index matches; the gate reports it unpaired so cutover leaves
// its name alone, and the constraint-backed primary key still pairs.
func TestGateCutoverReportsAnIndexTheChangeAddedAsUnpaired(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders ADD CONSTRAINT orders_qty_key UNIQUE (qty)`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)

	shadow := s.built.ShadowTable()
	assert.Equal(t, []schemachange.DependentPair{
		{Kind: schemachange.DependentIndex, SourceName: "orders_note_idx", ShadowName: shadow + "_note_idx"},
		{Kind: schemachange.DependentIndex, SourceName: "orders_pkey", ShadowName: shadow + "_pkey"},
		{Kind: schemachange.DependentIndex, SourceName: "orders_qty_idx", ShadowName: shadow + "_qty_idx"},
	}, ready.Indexes().Pairs)
	assert.Empty(t, ready.Indexes().UnpairedSource)
	assert.Equal(t, []string{"orders_qty_key"}, ready.Indexes().UnpairedShadow)
}

// The gate accepts only a verified shadow minted for the built shadow it is
// handed: a zero built proof, a zero verified proof, or a verified proof for
// another table is refused as unverified before any catalog read (CO-1).
func TestGateCutoverRefusesProofsThatDoNotDescribeTheSameCopy(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	f.exec(t, `
		CREATE TABLE %s.shipments (
			id  bigint PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.shipments SELECT g, g FROM generate_series(1, 50) g`)
	orders := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	shipments := f.stage(t, "shipments", `ALTER TABLE %s.shipments ALTER COLUMN qty TYPE bigint`)

	t.Run("zero built proof", func(t *testing.T) {
		_, err := schemachange.GateCutover(t.Context(), f.pool, orders.lock, schemachange.BuiltShadow{}, orders.verified, schemachange.Options{})
		assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err))
	})
	t.Run("zero verified proof", func(t *testing.T) {
		_, err := schemachange.GateCutover(t.Context(), f.pool, orders.lock, orders.built, checksum.VerifiedShadow{}, schemachange.Options{})
		assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err))
	})
	t.Run("verified proof for another table", func(t *testing.T) {
		_, err := schemachange.GateCutover(t.Context(), f.pool, orders.lock, orders.built, shipments.verified, schemachange.Options{})
		assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err))
	})
}

// The gate runs under the same per-table lock as every shadow operation
// (LK-1): it refuses with no session or a session for another table before
// connecting, and refuses from inside its transaction when the server no
// longer grants the lock to the session's backend.
func TestGateCutoverRequiresTheTableLock(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	f.exec(t, `
		CREATE TABLE %s.gadgets (
			id bigint PRIMARY KEY
		)`)
	stageLock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	s := f.stageUnder(t, stageLock, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	require.NoError(t, stageLock.Release(t.Context()))

	_, err = schemachange.GateCutover(t.Context(), f.pool, nil, s.built, s.verified, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "no lock")
	assert.Equal(t, schemachange.CauseLockUnproven, schemachange.RefusalCauseOf(err), "no lock")

	_, err = schemachange.GateCutover(t.Context(), f.pool, f.lock(t, "gadgets"), s.built, s.verified, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "lock for another table")
	assert.Equal(t, schemachange.CauseLockUnproven, schemachange.RefusalCauseOf(err), "lock for another table")

	_, err = schemachange.GateCutover(t.Context(), f.pool, f.goneLock(t, "orders"), s.built, s.verified, schemachange.Options{})
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation, "gone lock")
	assert.Equal(t, schemachange.CauseLockUnconfirmed, schemachange.RefusalCauseOf(err), "gone lock")
}

// A grant added to the source after the build is metadata the shadow does
// not carry; the gate refuses as fidelity drift rather than swapping in a
// table that would silently narrow access (ST-5).
func TestGateCutoverRefusesASourceGrantAddedAfterTheBuild(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	f.exec(t, `GRANT SELECT ON %s.orders TO PUBLIC`)

	_, err := f.gate(t, s)
	assert.Equal(t, schemachange.CauseFidelityDrift, schemachange.RefusalCauseOf(err))
}

// An index created on the source after the build changes the source's
// shape from the one the shadow was derived against; the gate refuses as
// schema drift so the change is re-planned against the table as it is now
// (ST-5).
func TestGateCutoverRefusesASourceIndexAddedAfterTheBuild(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	f.exec(t, `CREATE INDEX orders_sku_idx ON %s.orders (sku)`)

	_, err := f.gate(t, s)
	assert.Equal(t, schemachange.CauseSchemaDrift, schemachange.RefusalCauseOf(err))
}

// A concurrent unique build on the shadow that fails on duplicate values
// leaves an invalid index behind; the gate refuses before comparing shapes,
// so the invalid index is named rather than reported as drift (ST-5).
func TestGateCutoverRefusesAnInvalidShadowIndex(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	// sku takes seven values over 2500 rows, so the unique build fails and
	// CREATE INDEX CONCURRENTLY leaves the index invalid.
	_, err := f.pool.Exec(t.Context(), `CREATE UNIQUE INDEX CONCURRENTLY orders_shadow_sku_key ON `+f.shadowName(s.built)+` (sku)`)
	require.Error(t, err, "the unique build must fail on duplicate sku values")
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = 'orders_shadow_sku_key'`, f.schema).Scan(&valid))
	require.False(t, valid, "the failed concurrent build leaves an invalid index")

	_, err = f.gate(t, s)
	assert.Equal(t, schemachange.CauseIndexInvalid, schemachange.RefusalCauseOf(err))
}

// A relation that took the source's name after the build is not the table
// the proof describes, even with the same shape; the gate refuses on the
// moved OID (ST-6).
func TestGateCutoverRefusesAReplacedSource(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	f.exec(t, `ALTER TABLE %s.orders RENAME TO orders_was`)
	f.exec(t, `CREATE TABLE %s.orders (LIKE %s.orders_was INCLUDING ALL)`)

	_, err := f.gate(t, s)
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
}

// A relation already wearing the name the retained source takes after the
// swap — a leftover from an earlier run — would make the rename fail under
// the cutover lock; the gate refuses first, naming the taken name (ST-5).
func TestGateCutoverRefusesWhenTheOldNameIsTaken(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	f.exec(t, `CREATE TABLE %s.`+schemachange.OldName(f.schema, "orders")+` (leftover integer)`)

	_, err := f.gate(t, s)
	assert.Equal(t, schemachange.CauseNameTaken, schemachange.RefusalCauseOf(err))
}

// Renaming a constraint-backed shadow index to its source name renames the
// constraint with it, so a check constraint the gated statement named after
// the source's primary key would collide; the gate refuses. The same name
// borrowed from a plain index collides with nothing — a plain index rename
// touches no constraint — and passes.
func TestGateCutoverRefusesAShadowConstraintNamedAsAConstraintBackedSourceIndex(t *testing.T) {
	t.Run("named as the primary key", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders ADD CONSTRAINT orders_pkey CHECK (qty > 0)`)

		_, err := f.gate(t, s)
		assert.Equal(t, schemachange.CauseNameTaken, schemachange.RefusalCauseOf(err))
	})
	t.Run("named as a plain index", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders ADD CONSTRAINT orders_qty_idx CHECK (qty > 0)`)

		ready, err := f.gate(t, s)
		require.NoError(t, err)
		assert.Len(t, ready.Indexes().Pairs, 3)
	})
}
