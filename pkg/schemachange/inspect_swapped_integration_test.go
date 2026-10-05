//go:build integration || !unit

package schemachange_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// swapProofKeyPaths is the wire shape of an encoded SwapProof: every key at
// every depth, with `[]` marking descent into an array element. A
// checkpoint written after one cutover is decoded by the next run, so a
// renamed or untagged field anywhere in the tree is a compatibility break
// this list pins.
var swapProofKeyPaths = []string{
	"schema",
	"table",
	"old_table",
	"live_oid",
	"old_oid",
	"owner",
	"indexes",
	"indexes.pairs",
	"indexes.pairs[].kind",
	"indexes.pairs[].source_name",
	"indexes.pairs[].shadow_name",
	"indexes.unpaired_source",
	"indexes.unpaired_shadow",
	"statistics",
	"statistics.pairs",
	"statistics.pairs[].kind",
	"statistics.pairs[].source_name",
	"statistics.pairs[].shadow_name",
	"statistics.unpaired_source",
	"statistics.unpaired_shadow",
	"owned_sequences",
	"owned_sequences[].column",
	"owned_sequences[].sequence_schema",
	"owned_sequences[].sequence_name",
	"identity_columns",
	"identity_columns[].column",
	"identity_columns[].always",
	"identity_columns[].sequence_schema",
	"identity_columns[].sequence_name",
	"identity_columns[].options",
	"identity_columns[].options.start",
	"identity_columns[].options.increment",
	"identity_columns[].options.min",
	"identity_columns[].options.max",
	"identity_columns[].options.cache",
	"identity_columns[].options.cycle",
	"attempts",
}

// afterTheFact is the SwapProof a cutover returned as a later inspection
// can re-derive it: the pairing re-derived from the names the swap gave
// knows no shadow names and lists an unpaired source dependent under the
// derived name it bears, and no later read can know how many lock
// acquisitions the swap needed.
func afterTheFact(proof schemachange.SwapProof) schemachange.SwapProof {
	rederived := func(p schemachange.DependentPairing) schemachange.DependentPairing {
		pairs := make([]schemachange.DependentPair, 0, len(p.Pairs))
		for _, pair := range p.Pairs {
			pairs = append(pairs, schemachange.DependentPair{Kind: pair.Kind, SourceName: pair.SourceName})
		}
		unpaired := make([]string, 0, len(p.UnpairedSource))
		for _, name := range p.UnpairedSource {
			unpaired = append(unpaired, schemachange.OldDependentName(proof.Schema, proof.Table, name))
		}
		return schemachange.DependentPairing{Pairs: pairs, UnpairedSource: unpaired, UnpairedShadow: p.UnpairedShadow}
	}
	proof.Indexes = rederived(proof.Indexes)
	proof.Statistics = rederived(proof.Statistics)
	proof.Attempts = 0
	return proof
}

// A run that stops between Cutover and DropOldTable resumes by inspecting
// the swapped table from the Proof it checkpointed before the cutover.
// The inspection re-derives what the cutover returned — both OIDs, the
// owner, the dependent pairing by the names the swap gave (the index on
// the dropped column that had no partner is listed under the derived
// name it now bears), the re-owned sequence — and DropOldTable accepts
// the inspected value as it would the cutover's.
func TestInspectSwappedRederivesTheSwappedTableTheCutoverReturned(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	oldDependent := func(name string) string { return schemachange.OldDependentName(f.schema, "orders", name) }

	inspected, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	require.NoError(t, err)

	assert.Equal(t, afterTheFact(swapped.Proof()), inspected.Proof(), "the resume comparison: inspection re-derives the checkpointed proof")
	assert.Equal(t, s.built.ShadowOID(), inspected.LiveOID())
	assert.Equal(t, s.built.SourceOID(), inspected.OldOID())
	assert.Equal(t, s.built.Fidelity().Owner, inspected.Owner())
	assert.Equal(t, schemachange.DependentPairing{
		Pairs: []schemachange.DependentPair{
			{Kind: schemachange.DependentIndex, SourceName: "orders_pkey"},
			{Kind: schemachange.DependentIndex, SourceName: "orders_qty_idx"},
		},
		UnpairedSource: []string{oldDependent("orders_note_idx")},
		UnpairedShadow: []string{},
	}, inspected.Indexes())
	assert.Equal(t, []schemachange.OwnedSequence{
		{Column: "id", SequenceSchema: f.schema, SequenceName: "orders_id_seq"},
	}, inspected.OwnedSequences())
	assert.Equal(t, 0, inspected.Attempts())

	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, s.lock, inspected, schemachange.Options{}))
	assert.False(t, f.relationExists(t, inspected.OldTable()), "the inspected value drops the retained source")
	assert.Equal(t, int64(2500), f.rowCount(t, "orders"), "the live table is untouched")
}

// Identity columns the swap recreated are read back from the live table
// and checked against the handoff the proof recorded: the inspection
// reports the same identities the cutover did. The table also carries a
// serial column, an index, and an extended statistics object, so every
// nested type is present in the encoded SwapProof and the key-path walk
// sees each of its fields; Go's encoder and decoder agree by field name
// when a tag is missing, so equality of the two proofs alone would not
// notice a lost tag.
func TestInspectSwappedReadsBackTheIdentityHandoffAndRoundTripsAsJSON(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.tickets (
			id    bigint GENERATED ALWAYS AS IDENTITY (INCREMENT BY 2 START WITH 10) PRIMARY KEY,
			seqno bigserial,
			qty   integer NOT NULL,
			sku   text
		)`)
	f.exec(t, `CREATE INDEX tickets_qty_idx ON %s.tickets (qty)`)
	f.exec(t, `CREATE STATISTICS %s.tickets_qty_sku_stat ON qty, sku FROM %s.tickets`)
	f.exec(t, `INSERT INTO %s.tickets (qty, sku) SELECT g, 'sku' || (g % 7) FROM generate_series(1, 100) g`)
	s := f.stage(t, "tickets", `ALTER TABLE %s.tickets ALTER COLUMN qty TYPE bigint`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)

	inspected, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	require.NoError(t, err)

	require.NotEmpty(t, swapped.IdentityColumns(), "the fixture populates the identity handoff")
	assert.Equal(t, swapped.IdentityColumns(), inspected.IdentityColumns())
	assert.Equal(t, afterTheFact(swapped.Proof()), inspected.Proof())
	assert.Equal(t, f.schema+".tickets_id_seq", f.serialSequence(t, "tickets", "id"))
	require.NotEmpty(t, inspected.Indexes().Pairs, "the fixture populates the index pairing")
	require.NotEmpty(t, inspected.Statistics().Pairs, "the fixture populates the statistics pairing")
	require.NotEmpty(t, inspected.OwnedSequences(), "the fixture populates the re-owned sequences")

	encoded, err := json.Marshal(inspected)
	require.NoError(t, err)
	var decoded schemachange.SwapProof
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, inspected.Proof(), decoded, "the checkpoint decodes to the proof the inspection returned")
	var tree any
	require.NoError(t, json.Unmarshal(encoded, &tree))
	want := append([]string(nil), swapProofKeyPaths...)
	sort.Strings(want)
	assert.Equal(t, want, jsonKeyPaths(tree), "the checkpoint's wire shape is the SwapProof's field set at every depth")
}

// An identity column the swap recreated that is no longer an identity on
// the live table is not the table the swap left: the inspection refuses it
// as a swap mismatch rather than mint a proof that misdescribes the key.
func TestInspectSwappedRefusesALiveTableWhoseIdentityWasDropped(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.tickets (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.tickets (qty) SELECT g FROM generate_series(1, 100) g`)
	s := f.stage(t, "tickets", `ALTER TABLE %s.tickets ALTER COLUMN qty TYPE bigint`)
	_, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	f.exec(t, `ALTER TABLE %s.tickets ALTER COLUMN id DROP IDENTITY`)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseSwapMismatch, schemachange.RefusalCauseOf(err))
}

// Before any cutover the source still bears its name: the inspection says
// so distinctly, so a resume runs Cutover rather than treat it as a
// failure.
func TestInspectSwappedReportsASourceStillLive(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)

	_, err := schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrNotSwapped)
	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "the source is untouched")
}

// A run that stopped after DropOldTable committed finds the shadow live
// and nothing under the _old name: the inspection reports the old table
// missing, so the resume knows the swap is complete.
func TestInspectSwappedReportsAnOldTableAlreadyDropped(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	require.NoError(t, schemachange.DropOldTable(t.Context(), f.pool, s.lock, swapped, schemachange.Options{}))

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrOldTableNotFound)
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the live table is the shadow")
}

// The _old name borne by a relation other than the retained source is
// refused: a proof minted from it would let DropOldTable remove a table
// the swap never retained (ST-6).
func TestInspectSwappedRefusesAnImpostorUnderTheOldName(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	swapped, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	old := pgx.Identifier{swapped.OldTable()}.Sanitize()
	f.exec(t, `ALTER TABLE %s.`+old+` RENAME TO orders_retained`)
	f.exec(t, `CREATE TABLE %s.`+old+` (impostor integer)`)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})

	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
	assert.Equal(t, schemachange.CauseRelationReplaced, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, swapped.OldTable()), "the impostor is untouched")
	assert.True(t, f.relationExists(t, "orders_retained"), "and so is the retained source")
}

// The source's name borne by neither relation the build proved — by a
// third table, or by nothing — leaves the swap's outcome unreadable: the
// inspection refuses it as ambiguous rather than guess (LK-4).
func TestInspectSwappedRefusesALiveNameBorneByNeitherRelation(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	_, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)
	f.exec(t, `ALTER TABLE %s.orders RENAME TO orders_elsewhere`)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	assert.Equal(t, schemachange.CauseOutcomeAmbiguous, schemachange.RefusalCauseOf(err), "nothing bears the source's name")

	f.exec(t, `CREATE TABLE %s.orders (impostor integer)`)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, s.built.Proof(), schemachange.Options{})
	assert.Equal(t, schemachange.CauseOutcomeAmbiguous, schemachange.RefusalCauseOf(err), "a relation the build never proved bears the source's name")
}

// The inspection runs only under the table's lock session, like every
// other operation on the table (LK-1), and only from a proof that names a
// shadow: without either it refuses before touching the catalog.
func TestInspectSwappedRefusesAMissingLockAndAnEmptyProof(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	_, err := f.cutover(t, s, schemachange.CutoverOptions{})
	require.NoError(t, err)

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, nil, s.built.Proof(), schemachange.Options{})
	assert.Equal(t, schemachange.CauseLockUnproven, schemachange.RefusalCauseOf(err))

	_, err = schemachange.InspectSwapped(t.Context(), f.pool, s.lock, schemachange.Proof{}, schemachange.Options{})
	assert.Equal(t, schemachange.CauseProofEmpty, schemachange.RefusalCauseOf(err))
}
