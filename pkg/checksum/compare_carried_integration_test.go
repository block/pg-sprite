package checksum_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The compares below run the way the swap runs them: a live pass leaves
// keys it could not judge, and a READ COMMITTED transaction holding ACCESS
// EXCLUSIVE on the source and the shadow reads those keys on both sides.
// No catch-up runs in these fixtures, so the shadow changes only when a
// test writes it, and a test stands in for the drain by writing the shadow
// inside the locked transaction.

// touchOrders is the statement that keeps an orders row changing: %s is
// the fixture schema and $1 the keys.
const touchOrders = `
	UPDATE %s.orders
	SET qty = qty + 1
	WHERE id = ANY($1::bigint[])`

// leaveForSwap runs a live pass that is allowed to leave keys for the
// swap and returns the proof it mints naming exactly them. touch changes
// the source rows with those keys; it runs once before the pass and again
// each time the pass asks how far the catch-up has confirmed, so the keys
// read changed at every reread while the stand-in position claims
// everything is confirmed.
func (f verifierFixture) leaveForSwap(t *testing.T, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession, touch string, keys ...int64) checksum.VerifiedShadow {
	t.Helper()
	rewrite := func() {
		_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(touch, "%s", f.schema), keys)
		require.NoError(t, err)
	}
	rewrite()
	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows})
	require.NoError(t, err)
	outcome, err := v.CheckLive(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed: confirmedFunc(func() decode.LSN {
			rewrite()
			return math.MaxUint64
		}),
		Attempts:  2,
		CarryKeys: len(keys),
	})
	require.NoError(t, err)
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted, "the pass judged every key but the ones it leaves")
	require.Equal(t, keys, verified.Carried())
	return verified
}

// begin opens the READ COMMITTED transaction a swap runs in and rolls it
// back when the test ends.
func (f verifierFixture) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })
	return tx
}

// stilled opens a swap's transaction and takes ACCESS EXCLUSIVE on the
// source and the shadow in it, as the swap does before it compares.
func (f verifierFixture) stilled(t *testing.T, shadow schemachange.BuiltShadow) pgx.Tx {
	t.Helper()
	tx := f.begin(t)
	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()
	_, err := tx.Exec(t.Context(), "LOCK TABLE "+source+", "+f.shadowName(shadow)+" IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	return tx
}

// execIn runs SQL inside tx with %s standing for the fixture schema.
func (f verifierFixture) execIn(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	_, err := tx.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema), args...)
	require.NoError(t, err)
}

// setting reads one session setting inside tx.
func setting(t *testing.T, tx pgx.Tx, name string) string {
	t.Helper()
	var value string
	require.NoError(t, tx.QueryRow(t.Context(), `SELECT pg_catalog.current_setting($1)`, name).Scan(&value))
	return value
}

// Two keys kept changing through the pass are left for the swap. Under
// the swap's lock, with nothing applied to the shadow, both still differ
// and the compare names them in key order. Once the transaction writes
// the source's value for key 7 into the shadow, as the drain would, the
// compare names only key 1500; once it writes key 1500 too, every carried
// key is equal and the compare names none.
func TestCompareCarriedNamesTheKeysThatDifferUnderTheSwapLock(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7, 1500)
	tx := f.stilled(t, shadow)
	level := `
		UPDATE ` + f.shadowName(shadow) + ` AS held
		SET qty = source.qty
		FROM %s.orders AS source
		WHERE source.id = held.id
		  AND held.id = $1`

	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Equal(t, []int64{7, 1500}, differing, "the shadow holds neither key's latest source row")

	f.execIn(t, tx, level, 7)
	differing, err = checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Equal(t, []int64{1500}, differing, "key 7 now holds the source's row")

	f.execIn(t, tx, level, 1500)
	differing, err = checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Empty(t, differing, "every carried key holds the source's row")
}

// A carried key is compared as a row, present or absent, not only as a
// value. Key 7 deleted from the source while the shadow keeps its row
// differs; with the row deleted from the shadow too, as the stream's
// delete would leave it, the key is absent from both sides and equal.
// Key 1500 deleted from the shadow alone differs the other way round.
func TestCompareCarriedJudgesAKeyOnlyOneSideHolds(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7, 1500)
	tx := f.stilled(t, shadow)
	f.execIn(t, tx, `
		UPDATE `+f.shadowName(shadow)+` AS held
		SET qty = source.qty
		FROM %s.orders AS source
		WHERE source.id = held.id
		  AND held.id = ANY($1::bigint[])`, []int64{7, 1500})

	f.execIn(t, tx, `DELETE FROM %s.orders WHERE id = 7`)
	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Equal(t, []int64{7}, differing, "the shadow holds a row the source deleted")

	f.execIn(t, tx, `DELETE FROM `+f.shadowName(shadow)+` WHERE id = 7`)
	differing, err = checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Empty(t, differing, "a key neither side holds is equal")

	f.execIn(t, tx, `DELETE FROM `+f.shadowName(shadow)+` WHERE id = 1500`)
	differing, err = checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Equal(t, []int64{1500}, differing, "the source holds a row the shadow is missing")
}

// The source side of the compare is read through the statement's USING
// expression, as the pass's digests are (D7). The shadow column holds the
// amount in cents, a hundred times the source's text value, so no cast of
// the source value equals it: only a compare that evaluates the expression
// finds the row the transaction wrote through that expression equal.
func TestCompareCarriedReadsTheSourceThroughTheStatementsUsing(t *testing.T) {
	f := newVerifierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			amount text NOT NULL
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.ledger (id, amount)
		SELECT n, (n || '.25') FROM generate_series(1, %d) AS n`, rows))
	target := f.prove(t, "ledger")
	lock := f.lock(t, "ledger")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ALTER COLUMN amount TYPE integer USING (amount::numeric * 100)::integer`)
	require.Equal(t, map[string]string{"amount": "(amount::numeric * 100)::int"}, shadow.Conversions())
	f.copy(t, target, shadow, lock)
	verified := f.leaveForSwap(t, target, shadow, lock, `
		UPDATE %s.ledger
		SET amount = (amount::numeric + 1)::text
		WHERE id = ANY($1::bigint[])`, 7)
	tx := f.stilled(t, shadow)

	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Equal(t, []int64{7}, differing, "the shadow holds the cents of an amount the source has since raised")

	f.execIn(t, tx, `
		UPDATE `+f.shadowName(shadow)+` AS held
		SET amount = (source.amount::numeric * 100)::integer
		FROM %s.ledger AS source
		WHERE source.id = held.id
		  AND held.id = 7`)
	differing, err = checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Empty(t, differing, "the shadow holds the expression's value for the source row")
}

// The compare evaluates the conversion afresh on the source side, so an
// operator planted on the build's search_path after the pass — an integer
// * integer that answers zero, exactly the signature the expression
// spells — would have it hold the shadow against a value the statement
// never produced. The compare recomputes the build's fingerprint first and
// refuses as an invariant violation naming the drift (CO-9).
func TestCompareCarriedRefusesWhenAConversionResolvesDifferentlyThanAtTheBuild(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			DROP COLUMN note,
			ALTER COLUMN qty TYPE bigint USING (qty * 1)::bigint`)
	require.Equal(t, map[string]string{"qty": "(qty * 1)::bigint"}, shadow.Conversions())
	f.copy(t, target, shadow, lock)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7)
	f.exec(t, `
		CREATE FUNCTION %s.times_zero(integer, integer) RETURNS integer
			LANGUAGE sql IMMUTABLE
			AS 'SELECT 0'`)
	f.exec(t, `CREATE OPERATOR %s.* (LEFTARG = integer, RIGHTARG = integer, FUNCTION = %s.times_zero)`)
	tx := f.stilled(t, shadow)

	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.ErrorIs(t, err, checksum.ErrConversionDrift, "the breach names itself as drift")
	assert.Empty(t, differing, "nothing was compared through the planted operator")
}

// The compare reads each side in a statement of its own, so only ACCESS
// EXCLUSIVE on both relations makes the two reads one moment. A
// transaction that holds no lock, or the lock on the source alone, is
// refused as an invariant violation (CO-1) before either side is read.
func TestCompareCarriedRefusesWithoutAccessExclusiveOnBothRelations(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7)

	t.Run("no lock", func(t *testing.T) {
		tx := f.begin(t)
		_, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
		require.ErrorIs(t, err, checksum.ErrInvariantViolation)
		assert.ErrorContains(t, err, "(CO-1)")
		assert.ErrorContains(t, err, "the transaction holds it on 0 of the two")
	})
	t.Run("source alone", func(t *testing.T) {
		tx := f.begin(t)
		f.execIn(t, tx, `LOCK TABLE %s.orders IN ACCESS EXCLUSIVE MODE`)
		_, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
		require.ErrorIs(t, err, checksum.ErrInvariantViolation)
		assert.ErrorContains(t, err, "(CO-1)")
		assert.ErrorContains(t, err, "the transaction holds it on 1 of the two")
	})
}

// The carried keys are keys of the relations the pass read. A shadow
// renamed aside and replaced by another table of the same name, inside
// the locked transaction, is a relation no pass judged: the compare
// resolves the names again and refuses (ST-6) instead of reading it.
func TestCompareCarriedRefusesAShadowThatIsNoLongerTheProvenRelation(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7)
	tx := f.stilled(t, shadow)
	f.execIn(t, tx, `ALTER TABLE `+f.shadowName(shadow)+` RENAME TO orders_aside`)
	f.execIn(t, tx, `CREATE TABLE `+f.shadowName(shadow)+` (LIKE %s.orders_aside INCLUDING ALL)`)

	_, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.ErrorContains(t, err, "(ST-6)")
}

// The swap's later statements run in the same transaction, so the compare
// puts back the two output settings it pins for the reads. The caller's
// values here are ones the compare would never choose. The search_path is
// left as the compare's reads need it, pg_catalog alone for a copy with
// no conversion, for the caller to set as its own statements need.
func TestCompareCarriedRestoresTheCallersOutputSettings(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	verified := f.leaveForSwap(t, target, shadow, lock, touchOrders, 7)
	tx := f.stilled(t, shadow)
	f.execIn(t, tx, `SET LOCAL extra_float_digits = 1`)
	f.execIn(t, tx, `SET LOCAL bytea_output = 'escape'`)
	f.execIn(t, tx, `SET LOCAL search_path = %s`)

	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, differing)

	assert.Equal(t, "1", setting(t, tx, "extra_float_digits"))
	assert.Equal(t, "escape", setting(t, tx, "bytea_output"))
	assert.Equal(t, "pg_catalog", setting(t, tx, "search_path"))
}

// A proof that leaves no key has nothing for the swap to compare: the
// compare returns no key without a lock to check and without changing a
// setting of the caller's transaction.
func TestCompareCarriedTouchesNothingForAProofThatLeavesNoKey(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows})
	require.NoError(t, err)
	outcome, err := v.CheckLive(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64), checksum.DivergenceAbort, checksum.LiveOptions{
		Confirmed: confirmedFunc(func() decode.LSN { return math.MaxUint64 }),
	})
	require.NoError(t, err)
	verified, minted := outcome.VerifiedShadow()
	require.True(t, minted)
	require.Empty(t, verified.Carried())
	tx := f.begin(t)
	f.execIn(t, tx, `SET LOCAL search_path = %s`)

	differing, err := checksum.CompareCarried(t.Context(), tx, target, shadow, verified)
	require.NoError(t, err)
	assert.Empty(t, differing)
	assert.Equal(t, f.schema, setting(t, tx, "search_path"), "the transaction's settings are as the caller left them")
}
