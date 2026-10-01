package checksum_test

import (
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// check runs one pass over fixed chunks under policy.
func (f verifierFixture) check(t *testing.T, pool *pgxpool.Pool, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession, through copier.Watermark, policy checksum.DivergencePolicy) (checksum.Outcome, error) {
	t.Helper()
	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows})
	require.NoError(t, err)
	return v.Check(t.Context(), pool, through, policy)
}

// corrupt puts three differences into the shadow, one per chunk: a changed
// value in the middle chunk, a missing row in the first, and a row the
// source does not hold in the last.
func (f verifierFixture) corrupt(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	f.exec(t, "DELETE FROM "+f.shadowName(shadow)+" WHERE id = 5")
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")
	f.exec(t, "INSERT INTO "+f.shadowName(shadow)+" (id, qty) VALUES (2600, 2600)")
}

// shadowRow reads one shadow row's qty, and whether the row exists.
func (f verifierFixture) shadowRow(t *testing.T, shadow schemachange.BuiltShadow, id int64) (qty int64, exists bool) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(), "SELECT qty FROM "+f.shadowName(shadow)+" WHERE id = $1", id).Scan(&qty)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return qty, true
}

// assertCorrupted asserts the three differences corrupt made are still in
// the shadow, so a pass that must not write did not.
func (f verifierFixture) assertCorrupted(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	_, exists := f.shadowRow(t, shadow, 5)
	assert.False(t, exists, "the missing row is still missing")
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(0), qty, "the changed row still differs")
	_, exists = f.shadowRow(t, shadow, 2600)
	assert.True(t, exists, "the extra row is still there")
}

// assertNoProofs asserts the outcome minted neither proof (CO-2).
func assertNoProofs(t *testing.T, outcome checksum.Outcome) {
	t.Helper()
	_, minted := outcome.CleanWatermark()
	assert.False(t, minted, "no clean watermark")
	_, minted = outcome.VerifiedShadow()
	assert.False(t, minted, "no verified shadow")
}

// A clean pass through the complete watermark mints both proofs: the clean
// watermark, and the verified shadow naming the relations the pass compared
// (CO-1). Which policy was stated does not matter when nothing differed.
func TestCheckMintsBothProofsForACleanCompletePass(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort)
	require.NoError(t, err)
	assert.True(t, outcome.Clean())
	assert.Empty(t, outcome.Repairs)
	assert.Equal(t, 3, outcome.Report.Chunks)
	clean, minted := outcome.CleanWatermark()
	require.True(t, minted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), clean.Watermark())
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted)
	assert.Equal(t, f.schema, verified.Schema())
	assert.Equal(t, "orders", verified.Table())
	assert.Equal(t, shadow.ShadowTable(), verified.Shadow())
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), verified.Watermark())
	assert.False(t, verified.VerifiedAt().IsZero())
}

// A clean pass through a partial watermark proves only the prefix it read:
// it mints the clean watermark at that frontier and no verified shadow,
// because the keys above it were never compared (CO-1).
func TestCheckMintsOnlyTheCleanWatermarkForAPartialPass(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1600")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(1500), checksum.DivergenceAbort)
	require.NoError(t, err)
	assert.True(t, outcome.Clean(), "the difference at 1600 is above the watermark")
	clean, minted := outcome.CleanWatermark()
	require.True(t, minted)
	assert.Equal(t, copier.NewWatermark(1500), clean.Watermark())
	_, minted = outcome.VerifiedShadow()
	assert.False(t, minted, "a partial pass cannot vouch for the whole shadow")
}

// Under DivergenceAbort a difference ends the pass at its report: the
// error carries every differing chunk, no proof is minted, and the shadow
// is left exactly as the pass found it for the operator to inspect.
func TestCheckAbortsOnDivergenceAndLeavesTheShadowAlone(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort)
	var divergence *checksum.DivergenceError
	require.ErrorAs(t, err, &divergence)
	require.Len(t, divergence.Report.Mismatches, 3)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), divergence.Report.Mismatches[0].Chunk)
	assert.Equal(t, chunk(t, 1001, 2000), divergence.Report.Mismatches[1].Chunk)
	assert.Equal(t, chunk(t, 2001, math.MaxInt64), divergence.Report.Mismatches[2].Chunk)
	assert.EqualError(t, err, "verify "+f.schema+".orders: source and shadow differ in 3 of 3 chunks through watermark 9223372036854775807")
	assert.False(t, outcome.Clean(), "a pass that found a difference is not clean")
	assert.Equal(t, divergence.Report, outcome.Report, "the outcome carries the comparison too")
	assert.Empty(t, outcome.Repairs)
	assertNoProofs(t, outcome)
	f.assertCorrupted(t, shadow)
}

// Under DivergenceRepair every differing chunk is recopied from the source
// and read again: the outcome lists each repair with what it removed and
// put back, the shadow equals the source afterwards, and the pass still
// mints no proof (CO-2). The next pass, reading every chunk fresh, finds
// the shadow clean and mints both.
func TestCheckRepairsEveryDifferingChunkAndMintsNothing(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	assert.False(t, outcome.Clean(), "the report is the comparison before the repairs")
	require.Len(t, outcome.Repairs, 3)
	missing, changed, extra := outcome.Repairs[0], outcome.Repairs[1], outcome.Repairs[2]
	assert.Equal(t, chunk(t, math.MinInt64, 1000), missing.Mismatch.Chunk)
	assert.Equal(t, int64(999), missing.Removed, "the shadow held 999 of the chunk's 1000 keys")
	assert.Equal(t, int64(1000), missing.Inserted)
	assert.Equal(t, chunk(t, 1001, 2000), changed.Mismatch.Chunk)
	assert.Equal(t, int64(1000), changed.Removed)
	assert.Equal(t, int64(1000), changed.Inserted)
	assert.Equal(t, chunk(t, 2001, math.MaxInt64), extra.Mismatch.Chunk)
	assert.Equal(t, int64(501), extra.Removed, "the extra row goes with the chunk")
	assert.Equal(t, int64(500), extra.Inserted, "the source holds 500 keys above 2000")
	assertNoProofs(t, outcome)

	_, exists := f.shadowRow(t, shadow, 5)
	assert.True(t, exists, "the missing row is back")
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(1500), qty, "the changed row holds the source's value")
	_, exists = f.shadowRow(t, shadow, 2600)
	assert.False(t, exists, "the extra row is gone")

	again, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	require.NoError(t, err)
	assert.True(t, again.Clean())
	assert.Empty(t, again.Repairs)
	_, minted := again.CleanWatermark()
	assert.True(t, minted, "the fresh pass mints the clean watermark")
	_, minted = again.VerifiedShadow()
	assert.True(t, minted, "and the verified shadow")
}

// A repair that does not take is refused, not retried: something other
// than the copier writes the shadow — here a trigger that zeroes every
// inserted qty — and recopying again would not say why. The recopy has
// committed before the fresh read finds it wrong, so the chunk is left as
// the trigger made it; the error names the chunk and the second reading.
func TestCheckRefusesARepairThatDidNotTake(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")
	f.exec(t, `
		CREATE FUNCTION %s.zero_qty() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.qty := 0;
			RETURN NEW;
		END
		$$`)
	f.exec(t, "CREATE TRIGGER zero_qty BEFORE INSERT ON "+f.shadowName(shadow)+" FOR EACH ROW EXECUTE FUNCTION %s.zero_qty()")

	outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	var failed *checksum.RepairError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, chunk(t, 1001, 2000), failed.Repair.Mismatch.Chunk)
	assert.Equal(t, int64(1000), failed.Repair.Removed)
	assert.Equal(t, int64(1000), failed.Repair.Inserted)
	assert.Equal(t, chunk(t, 1001, 2000), failed.After.Chunk)
	assert.Equal(t, int64(1000), failed.After.Source.Rows)
	assert.Equal(t, int64(1000), failed.After.Shadow.Rows, "every row is back, with the wrong value")
	assert.NotEqual(t, failed.After.Source.Hash, failed.After.Shadow.Hash)
	assert.EqualError(t, err, "verify "+f.schema+".orders: chunk [1001, 2000] still differs after its repair: source 1000 rows, shadow 1000 rows")
	assert.False(t, outcome.Clean())
	assert.Equal(t, []checksum.Repair{failed.Repair}, outcome.Repairs, "the committed recopy is reported with the refusal")
	assertNoProofs(t, outcome)

	var zeroed int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+f.shadowName(shadow)+" WHERE id BETWEEN 1001 AND 2000 AND qty = 0").Scan(&zeroed))
	assert.Equal(t, int64(1000), zeroed, "the committed recopy stands as the trigger wrote it")
}

// A pass must say what a difference means before it reads anything: the
// zero policy and a string that is not one of the two are refused with no
// pass run and no write made (CO-3).
func TestCheckRefusesAPassWithoutAPolicy(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.corrupt(t, shadow)

	for name, policy := range map[string]checksum.DivergencePolicy{
		"zero policy":    "",
		"unknown policy": "fix",
	} {
		t.Run(name, func(t *testing.T) {
			outcome, err := f.check(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), policy)
			require.ErrorIs(t, err, checksum.ErrNoDivergencePolicy)
			assert.Zero(t, outcome.Report.Chunks, "no pass ran")
			assert.False(t, outcome.Clean(), "a pass that compared nothing is not clean")
			assertNoProofs(t, outcome)
		})
	}
	f.assertCorrupted(t, shadow)
}
