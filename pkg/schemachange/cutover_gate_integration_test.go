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

// A verified-shadow proof minted for a shadow that was since dropped does
// not prove the shadow rebuilt under the same derived name: the rebuild is
// a new relation no copy has filled, and the gate refuses it on the OIDs
// the proof carries, since the names alone agree (CO-1).
func TestGateCutoverRefusesAVerifiedProofForAnEarlierShadow(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	require.NoError(t, schemachange.DropShadow(t.Context(), f.pool, s.lock, s.target, schemachange.Options{}))
	rebuilt, err := schemachange.BuildShadow(t.Context(), f.pool, s.lock, s.target, f.alter(t, `ALTER TABLE %s.orders DROP COLUMN note`), schemachange.Options{})
	require.NoError(t, err)
	require.Equal(t, s.built.ShadowTable(), rebuilt.ShadowTable(), "the rebuild wears the same derived name")
	require.NotEqual(t, s.built.ShadowOID(), rebuilt.ShadowOID(), "the rebuild is a new relation")

	_, err = schemachange.GateCutover(t.Context(), f.pool, s.lock, rebuilt, s.verified, schemachange.Options{})
	assert.Equal(t, schemachange.CauseCutoverUnverified, schemachange.RefusalCauseOf(err), "the rebuilt shadow holds no rows the pass compared")
}

// A privilege revoked on the source after the build is still granted on the
// shadow. A proof re-derived through InspectShadow records each table as it
// is, so neither side drifts from it and the fingerprints equal the build's;
// the gate still refuses, because the two tables' grants — which the gated
// statement cannot change — no longer agree (ST-5).
func TestGateCutoverAfterInspectionRefusesASourceRevoke(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	f.exec(t, `GRANT SELECT ON %s.orders TO PUBLIC`)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	f.exec(t, `REVOKE SELECT ON %s.orders FROM PUBLIC`)

	inspected, err := schemachange.InspectShadow(t.Context(), f.pool, s.lock, s.target, schemachange.Options{})
	require.NoError(t, err)
	require.Equal(t, s.built.SourceFingerprint(), inspected.SourceFingerprint())
	require.Equal(t, s.built.TargetFingerprint(), inspected.TargetFingerprint())

	_, err = schemachange.GateCutover(t.Context(), f.pool, s.lock, inspected, s.verified, schemachange.Options{})
	assert.Equal(t, schemachange.CauseFidelityDrift, schemachange.RefusalCauseOf(err), "the shadow still grants SELECT to PUBLIC")
}

// The checklist holds the shadow to its own build-time record as well as
// the source: a shadow replaced under its name, reshaped, or given metadata
// after the build is refused, and so is a source identity whose sequence
// options changed (ST-5, ST-6). Metadata the gated statement itself set on
// the shadow is what the record expects, and passes.
func TestGateCutoverHoldsTheShadowAndTheIdentitiesToTheBuildRecord(t *testing.T) {
	createAccounts := func(t *testing.T, f shadowFixture) {
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id  bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
				qty integer NOT NULL
			)`)
		f.exec(t, `INSERT INTO %s.accounts (qty) SELECT g FROM generate_series(1, 2500) g`)
	}
	refusals := []struct {
		name  string
		after func(t *testing.T, f shadowFixture, s staged)
		want  schemachange.RefusalCause
	}{
		{"shadow replaced", func(t *testing.T, f shadowFixture, s staged) {
			f.exec(t, "ALTER TABLE "+f.shadowName(s.built)+" RENAME TO shadow_was")
			f.exec(t, "CREATE TABLE "+f.shadowName(s.built)+" (LIKE %s.shadow_was INCLUDING ALL)")
		}, schemachange.CauseRelationReplaced},
		{"shadow reshaped", func(t *testing.T, f shadowFixture, s staged) {
			f.exec(t, "ALTER TABLE "+f.shadowName(s.built)+" ADD COLUMN extra integer")
		}, schemachange.CauseSchemaDrift},
		{"shadow metadata touched", func(t *testing.T, f shadowFixture, s staged) {
			f.exec(t, "COMMENT ON TABLE "+f.shadowName(s.built)+" IS 'touched'")
		}, schemachange.CauseFidelityDrift},
		{"source identity options", func(t *testing.T, f shadowFixture, s staged) {
			f.exec(t, `ALTER TABLE %s.accounts ALTER COLUMN id SET INCREMENT BY 5`)
		}, schemachange.CauseFidelityDrift},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			f := newShadowFixture(t)
			createAccounts(t, f)
			s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN qty TYPE bigint`)
			c.after(t, f, s)
			_, err := f.gate(t, s)
			assert.Equal(t, c.want, schemachange.RefusalCauseOf(err), "%v", err)
		})
	}
	t.Run("metadata the statement set", func(t *testing.T) {
		f := newShadowFixture(t)
		createAccounts(t, f)
		s := f.stage(t, "accounts", `ALTER TABLE %s.accounts SET (fillfactor = 50)`)
		require.Equal(t, []string{"fillfactor=50"}, s.built.ShadowFidelity().RelOptions, "the shadow's record is the statement's result")
		require.Empty(t, s.built.Fidelity().RelOptions, "the source's record is the source's")

		_, err := f.gate(t, s)
		assert.NoError(t, err)
	})
}

// Two unique indexes on one column that differ only in NULLS NOT DISTINCT
// are different indexes — one admits a second NULL, the other refuses it —
// and each pairs with the shadow copy that has the same setting, whatever
// order the catalog lists them in (D8). The clause exists from PostgreSQL 15.
func TestGateCutoverPairsIndexesOnNullsNotDistinct(t *testing.T) {
	f := newShadowFixture(t)
	if f.serverVersionNum(t) < 150000 {
		t.Skip("NULLS NOT DISTINCT exists from PostgreSQL 15")
	}
	f.exec(t, `
		CREATE TABLE %s.items (
			id    bigint PRIMARY KEY,
			code  text,
			other integer
		)`)
	f.exec(t, `CREATE UNIQUE INDEX z_nnd ON %s.items (code) NULLS NOT DISTINCT`)
	f.exec(t, `CREATE UNIQUE INDEX a_plain ON %s.items (code)`)
	f.exec(t, `INSERT INTO %s.items SELECT g, 'c' || g, g FROM generate_series(1, 50) g`)
	s := f.stage(t, "items", `ALTER TABLE %s.items DROP COLUMN other`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)
	nullsNotDistinct := func(name string) bool {
		var nnd bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT i.indnullsnotdistinct FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, f.schema, name).Scan(&nnd))
		return nnd
	}
	require.Len(t, ready.Indexes().Pairs, 3)
	for _, pair := range ready.Indexes().Pairs {
		assert.Equal(t, nullsNotDistinct(pair.SourceName), nullsNotDistinct(pair.ShadowName),
			"%s pairs with %s", pair.SourceName, pair.ShadowName)
	}
}

// Two indexes on one column that differ only in operator class are
// different indexes — text_pattern_ops serves LIKE prefixes, the default
// class serves ordering — and each pairs with the shadow copy of the same
// class (D8).
func TestGateCutoverPairsIndexesOnOperatorClass(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.items (
			id    bigint PRIMARY KEY,
			code  text,
			other integer
		)`)
	f.exec(t, `CREATE INDEX z_pattern ON %s.items (code text_pattern_ops)`)
	f.exec(t, `CREATE INDEX a_default ON %s.items (code)`)
	f.exec(t, `INSERT INTO %s.items SELECT g, 'c' || g, g FROM generate_series(1, 50) g`)
	s := f.stage(t, "items", `ALTER TABLE %s.items DROP COLUMN other`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)
	opclass := func(name string) string {
		var class string
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT oc.opcname FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_opclass oc ON oc.oid = i.indclass[0]
			WHERE n.nspname = $1 AND c.relname = $2`, f.schema, name).Scan(&class))
		return class
	}
	require.Len(t, ready.Indexes().Pairs, 3)
	for _, pair := range ready.Indexes().Pairs {
		assert.Equal(t, opclass(pair.SourceName), opclass(pair.ShadowName), "%s pairs with %s", pair.SourceName, pair.ShadowName)
	}
}

// Two extended-statistics objects on the same columns that differ only in
// the kinds they collect are different objects, and each pairs with the
// shadow copy collecting the same kinds (D8).
func TestGateCutoverPairsStatisticsOnKinds(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.items (
			id    bigint PRIMARY KEY,
			code  text,
			qty   integer,
			other integer
		)`)
	f.exec(t, `CREATE STATISTICS %s.z_distinct (ndistinct) ON code, qty FROM %s.items`)
	f.exec(t, `CREATE STATISTICS %s.a_dependencies (dependencies) ON code, qty FROM %s.items`)
	f.exec(t, `INSERT INTO %s.items SELECT g, 'c' || g, g, g FROM generate_series(1, 50) g`)
	s := f.stage(t, "items", `ALTER TABLE %s.items DROP COLUMN other`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)
	kinds := func(name string) string {
		var kinds string
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT array_to_string(s.stxkind, ',') FROM pg_statistic_ext s
			JOIN pg_namespace n ON n.oid = s.stxnamespace
			WHERE n.nspname = $1 AND s.stxname = $2`, f.schema, name).Scan(&kinds))
		return kinds
	}
	require.Len(t, ready.Statistics().Pairs, 2)
	for _, pair := range ready.Statistics().Pairs {
		assert.Equal(t, kinds(pair.SourceName), kinds(pair.ShadowName), "%s pairs with %s", pair.SourceName, pair.ShadowName)
	}
}

// The derived _old names the swap must find free cover the source's
// dependents and its identity sequences, not only the table: a leftover
// wearing the _old name of the source's statistics object, or of its
// identity sequence, is refused like one wearing the table's (ST-5).
func TestGateCutoverRefusesWhenADependentOrSequenceOldNameIsTaken(t *testing.T) {
	t.Run("statistics object", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

		f.exec(t, `CREATE TABLE %s.`+schemachange.OldDependentName(f.schema, "orders", "orders_qty_sku_stat")+` (leftover integer)`)

		_, err := f.gate(t, s)
		assert.Equal(t, schemachange.CauseNameTaken, schemachange.RefusalCauseOf(err))
	})
	t.Run("identity sequence", func(t *testing.T) {
		f := newShadowFixture(t)
		f.exec(t, `
			CREATE TABLE %s.accounts (
				id  bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
				qty integer NOT NULL
			)`)
		f.exec(t, `INSERT INTO %s.accounts (qty) SELECT g FROM generate_series(1, 2500) g`)
		s := f.stage(t, "accounts", `ALTER TABLE %s.accounts ALTER COLUMN qty TYPE bigint`)
		require.Equal(t, "accounts_id_seq", s.built.IdentityColumns()[0].SequenceName)

		f.exec(t, `CREATE TABLE %s.`+schemachange.OldDependentName(f.schema, "accounts", "accounts_id_seq")+` (leftover integer)`)

		_, err := f.gate(t, s)
		assert.Equal(t, schemachange.CauseNameTaken, schemachange.RefusalCauseOf(err))
	})
}

// A sequence owned by a column the gated statement dropped has no column
// on the live table to be re-owned to; the gate lists for re-owning only
// the sequences of columns the shadow kept, and the dropped column's goes
// with the old table (D5).
func TestGateCutoverListsOnlyTheOwnedSequencesOfKeptColumns(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id        bigserial PRIMARY KEY,
			legacy_id bigserial,
			qty       integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.orders (qty) SELECT g FROM generate_series(1, 2500) g`)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN legacy_id`)

	ready, err := f.gate(t, s)
	require.NoError(t, err)
	assert.Equal(t, []schemachange.OwnedSequence{
		{Column: "id", SequenceSchema: f.schema, SequenceName: "orders_id_seq"},
	}, ready.OwnedSequences())
}

// serverVersionNum is the server's version as server_version_num reports it.
func (f shadowFixture) serverVersionNum(t *testing.T) int {
	t.Helper()
	var version int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	return version
}
