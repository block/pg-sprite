package schemachange_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// leaveStoppedRun builds the shadow for alter and writes the row the Runner
// writes after building it — in PhaseCopying with nothing landed — with
// mutate applied to the row first, then releases the table so the test's
// own Run can take it. It is the state a run that stopped right after its
// first save leaves behind, bent as the test needs.
func (f runnerFixture) leaveStoppedRun(t *testing.T, alter string, mutate func(*checkpoint.Checkpoint)) schemachange.BuiltShadow {
	t.Helper()
	lock := f.lock(t, "orders")
	st := f.alter(t, alter)
	built, err := schemachange.BuildShadow(t.Context(), f.pool, lock, f.prove(t, "orders"), st, schemachange.Options{})
	require.NoError(t, err)
	proof, err := json.Marshal(built)
	require.NoError(t, err)
	row := checkpoint.Checkpoint{
		Schema:            f.schema,
		Table:             "orders",
		ShadowTable:       built.ShadowTable(),
		SourceFingerprint: built.SourceFingerprint(),
		TargetFingerprint: built.TargetFingerprint(),
		Phase:             checkpoint.PhaseCopying,
		ShadowProof:       proof,
		Statement:         st.SQL(),
	}
	mutate(&row)
	require.NoError(t, f.store.Ensure(t.Context()))
	require.NoError(t, f.store.Save(t.Context(), lock, row))
	require.NoError(t, lock.Release(t.Context()))
	return built
}

// A relation already under the shadow's name with no checkpoint row is
// refused as shadow-unrecorded (ST-2): nothing records which statement
// built it, so the run must not adopt it. The relation is left for the
// operator to drop.
func TestRunnerRefusesAShadowNoRowRecords(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	shadow := schemachange.ShadowName(f.schema, "orders")
	f.exec(t, `CREATE TABLE %s.`+shadow+` (id bigint PRIMARY KEY)`)

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseShadowUnrecorded, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrShadowExists)
	assert.True(t, f.relationExists(t, shadow), "the refused run leaves the unrecorded relation in place")
	_, err = f.store.Inspect(t.Context(), f.schema, "orders")
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "the refused run writes no row")
}

// A row that names a replication slot belongs to a run that decodes WAL;
// the quiesced runner refuses it as resume-row-decoding (ST-3) rather than
// continue without the slot and leave it, and the changes it carried,
// behind.
func TestRunnerRefusesARowThatNamesASlot(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		row.SlotName = "pgsprite_orders"
		row.PublicationName = "pgsprite_orders"
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowDecoding, schemachange.RefusalCauseOf(err))
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the refused run leaves the row as it found it")
}

// A row in a phase only a run that decodes WAL enters is refused the same
// way, even without a slot name.
func TestRunnerRefusesARowInADecodingPhase(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		row.Phase = checkpoint.PhaseCatchingUp
		row.Watermark = copier.NewWatermark(math.MaxInt64)
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowDecoding, schemachange.RefusalCauseOf(err))
}

// A row whose fingerprints are another statement's than the shadow under
// the derived name embodies is refused as resume-row-mismatch: the row and
// the shadow disagree about what is being built, and neither is trusted
// over the other.
func TestRunnerRefusesARowWhoseFingerprintsAreNotTheShadows(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	built := f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		row.TargetFingerprint = "another statement's shape"
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, built.ShadowTable()), "the refused run drops nothing")
}

// A row whose shadow proof is not the one the shadow under its name
// re-derives is refused as resume-row-mismatch even when the names and
// fingerprints agree: the proof is what the swap moves by, so a row that
// carries another shadow's proof must not drive the swap.
func TestRunnerRefusesARowWhoseProofIsAnotherShadows(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		var proof schemachange.Proof
		require.NoError(t, json.Unmarshal(row.ShadowProof, &proof))
		proof.ShadowOID++
		var err error
		row.ShadowProof, err = json.Marshal(proof)
		require.NoError(t, err)
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
}

// A row without a shadow proof cannot be resumed: without it a resume
// past the swap would not know which relations moved.
func TestRunnerRefusesARowWithoutAShadowProof(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		row.ShadowProof = nil
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
}

// A row in copying whose shadow has been dropped is an error, not a
// refusal class of its own: the run cannot continue and cannot start over
// while the row stands, and the error says to delete the row.
func TestRunnerFailsACopyingRowWhoseShadowIsGone(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	built := f.leaveStoppedRun(t, alter, func(*checkpoint.Checkpoint) {})
	f.exec(t, `DROP TABLE %s.`+built.ShadowTable())

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.ErrorIs(t, err, schemachange.ErrShadowNotFound)
	assert.Empty(t, schemachange.RefusalCauseOf(err), "a missing shadow is not a refusal class")
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the row stands for the operator to delete")
}

// A row records the statement whose run it is, and a resume under another
// statement is refused as resume-row-mismatch before anything is touched:
// a resume completes the change the stopped run started, never the one
// the caller changed its mind to. The shadow and the row stand, for the
// first statement's run to resume or the operator to abandon.
func TestRunnerRefusesAResumeUnderAnotherStatement(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.leaveStoppedRun(t, `ALTER TABLE %s.orders DROP COLUMN note`, func(*checkpoint.Checkpoint) {})

	_, err := f.run(t, "orders", `ALTER TABLE %s.orders DROP COLUMN sku`, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
	assert.True(t, f.relationExists(t, built.ShadowTable()), "the refused run drops nothing")
	assert.Equal(t, checkpoint.PhaseCopying, f.inspectRow(t, "orders").Phase, "the refused run leaves the row as it found it")
	assert.True(t, f.hasColumn(t, "orders", "sku"), "nothing was built for the second statement")
}

// The statements are compared as parsed, not as typed: a resume whose
// statement differs from the row's only in case, whitespace, and whether
// the table is qualified with the target's schema is the same change and
// resumes.
func TestRunnerResumesUnderTheSameStatementSpelledDifferently(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	built := f.leaveStoppedRun(t, `ALTER TABLE %s.orders DROP COLUMN note`, func(*checkpoint.Checkpoint) {})

	result := f.assertResumedToDone(t, `alter table   orders
		drop column note`, schemachange.RunOptions{})
	assert.Equal(t, built.ShadowOID(), result.Swapped.LiveOID, "the resumed run continued on the shadow the first built")
}

// A row that records no statement cannot prove which change it is a run
// of, and is refused as resume-row-mismatch for the operator to delete.
func TestRunnerRefusesARowWithoutAStatement(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	const alter = `ALTER TABLE %s.orders DROP COLUMN note`
	f.leaveStoppedRun(t, alter, func(row *checkpoint.Checkpoint) {
		row.Statement = ""
	})

	_, err := f.run(t, "orders", alter, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseResumeRowMismatch, schemachange.RefusalCauseOf(err))
}

// A statement that names another table than the proven one is refused as
// shadow-statement-target on resume as it is on a fresh build, before the
// row's statement is compared with it.
func TestRunnerRefusesAResumeWhoseStatementNamesAnotherTable(t *testing.T) {
	f := newRunnerFixture(t)
	f.createOrders(t)
	f.leaveStoppedRun(t, `ALTER TABLE %s.orders DROP COLUMN note`, func(*checkpoint.Checkpoint) {})

	_, err := f.run(t, "orders", `ALTER TABLE %s.invoices DROP COLUMN note`, schemachange.RunOptions{})
	require.Error(t, err)
	assert.Equal(t, schemachange.CauseStatementTarget, schemachange.RefusalCauseOf(err))
}
